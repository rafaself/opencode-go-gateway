package server

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/codex"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

const (
	featureNotImplementedCode = "feature_not_implemented"
	featureNotImplementedText = "This Responses feature is not implemented in the text-only milestone."
	requestInvalidText        = "The request could not be processed."
	requestUnsupportedText    = "The request uses an unsupported feature."
	requestTooLargeText       = "The request body exceeds the configured limit."
	upstreamFailureText       = "The upstream provider request failed."
	upstreamContentTypeText   = "The upstream provider returned an unsupported content type."
	upstreamNotConfiguredText = "The upstream provider is not configured."
)

func (s *Server) handleResponses(w *statusWriter, r *http.Request) {
	if r.Body == nil {
		writeJSONError(w, http.StatusBadRequest, string(codex.ErrorInvalidRequest), requestInvalidText)
		return
	}
	defer r.Body.Close()

	decoder, err := codex.NewDecoder(s.config.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "server_configuration_error", "the gateway is not configured correctly")
		return
	}
	request, err := decoder.DecodeRequest(r)
	if err != nil {
		writeRequestDecodeError(w, err)
		return
	}
	if err := validateTextOnlyRequest(request); err != nil {
		writeJSONError(w, http.StatusNotImplemented, featureNotImplementedCode, err.Error())
		return
	}

	upstreamContext, cancel := context.WithCancel(r.Context())
	defer cancel()
	response, err := s.upstream.Do(upstreamContext, request)
	if err != nil {
		closeUpstreamResponse(response)
		writeUpstreamError(w, err)
		return
	}
	if err := validateUpstreamResponse(response); err != nil {
		closeUpstreamResponse(response)
		writeUpstreamError(w, err)
		return
	}

	bodyCloser := &onceCloser{body: response.Body}
	defer bodyCloser.Close()

	session, err := codex.NewStreamSession(w, codex.StreamSessionOptions{
		Model: opencodego.DefaultModel,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "stream_configuration_error", "the response stream could not be initialized")
		return
	}
	w.responseModel = session.Model
	if err := session.Start(); err != nil {
		return
	}
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	go watchUpstreamOwnership(stopWatcher, r.Context().Done(), session.Done(), cancel, bodyCloser.Close, watcherDone)
	defer func() {
		close(stopWatcher)
		<-watcherDone
	}()

	streamDecoder := opencodego.NewBridgeStreamDecoder(response.Body, opencodego.SSEDecoderOptions{
		MaxAggregateBytes: opencodego.DefaultStreamMaxAggregateBytes,
	})
	for {
		event, decodeErr := streamDecoder.Next()
		if errors.Is(decodeErr, io.EOF) {
			if !session.TerminalEmitted && session.WriteFailure() == nil {
				_ = session.Fail("upstream_eof", "The upstream response stream ended unexpectedly.")
				w.responseTerminal = "response.failed"
			}
			return
		}
		if decodeErr != nil {
			if !session.TerminalEmitted && session.WriteFailure() == nil {
				_ = session.Fail("upstream_stream_error", "The upstream response stream could not be decoded.")
				w.responseTerminal = "response.failed"
			}
			return
		}
		if handleErr := session.Handle(event); handleErr != nil {
			if session.TerminalEmitted {
				w.responseTerminal = responseTerminalType(event)
			}
			return
		}
		if session.TerminalEmitted {
			w.responseTerminal = responseTerminalType(event)
			return
		}
	}
}

func responseTerminalType(event bridge.StreamEvent) string {
	switch event.(type) {
	case bridge.Completed, *bridge.Completed:
		return "response.completed"
	case bridge.Incomplete, *bridge.Incomplete:
		return "response.incomplete"
	case bridge.Failed, *bridge.Failed:
		return "response.failed"
	default:
		return "response.failed"
	}
}

func validateTextOnlyRequest(request bridge.Request) error {
	if request.PreviousResponseID != "" {
		return errors.New("continuation state is not implemented in the text-only milestone")
	}
	if len(request.Tools) > 0 {
		return errors.New(featureNotImplementedText)
	}
	for _, item := range request.Input {
		if item == nil {
			return errors.New(featureNotImplementedText)
		}
		if item.Kind() != bridge.InputMessage {
			return errors.New(featureNotImplementedText)
		}
	}
	switch request.ToolChoice.Kind {
	case bridge.ToolChoiceUnset, bridge.ToolChoiceAuto, bridge.ToolChoiceNone:
	default:
		return errors.New(featureNotImplementedText)
	}
	if format := request.Generation.Text.Format.Kind; format != "" && format != bridge.TextFormatText {
		return errors.New("structured response formats are not implemented in the text-only milestone")
	}
	return nil
}

func writeRequestDecodeError(w *statusWriter, err error) {
	var decodeError *codex.Error
	if !errors.As(err, &decodeError) {
		writeJSONError(w, http.StatusBadRequest, string(codex.ErrorInvalidRequest), requestInvalidText)
		return
	}
	status := http.StatusBadRequest
	message := requestInvalidText
	switch decodeError.Code {
	case codex.ErrorRequestTooLarge:
		status = http.StatusRequestEntityTooLarge
		message = requestTooLargeText
	case codex.ErrorUnsupportedField, codex.ErrorUnsupportedItemType, codex.ErrorUnsupportedToolType:
		status = http.StatusNotImplemented
		message = requestUnsupportedText
	}
	writeJSONError(w, status, string(decodeError.Code), message)
}

func validateUpstreamResponse(response *UpstreamResponse) error {
	if response == nil {
		return &UpstreamError{Code: upstreamErrorBadGateway}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &UpstreamError{Code: upstreamCodeForStatus(response.StatusCode), StatusCode: response.StatusCode}
	}
	if response.Body == nil {
		return &UpstreamError{Code: upstreamErrorMalformed}
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, opencodego.DefaultAccept) {
		return &UpstreamError{Code: upstreamErrorContentType}
	}
	return nil
}

func upstreamCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return upstreamErrorBadRequest
	case http.StatusUnauthorized:
		return upstreamErrorUnauthorized
	case http.StatusForbidden:
		return upstreamErrorForbidden
	case http.StatusTooManyRequests:
		return upstreamErrorRateLimited
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return upstreamErrorServer
	default:
		return upstreamErrorUnexpected
	}
}

func writeUpstreamError(w *statusWriter, err error) {
	code := upstreamErrorBadGateway
	status := http.StatusBadGateway
	retryAfter := ""
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		code = safeUpstreamCode(upstreamErr.Code)
		retryAfter = upstreamErr.RetryAfter
		if code == upstreamErrorRateLimited {
			status = http.StatusTooManyRequests
		}
		if code == upstreamErrorInvalidRequest || code == upstreamErrorTooLarge || code == upstreamErrorUnsupportedTool {
			status = http.StatusBadRequest
		}
		if code == upstreamErrorNotConfigured {
			status = http.StatusInternalServerError
		}
	}
	if retryAfter != "" && code == upstreamErrorRateLimited {
		w.Header().Set("Retry-After", retryAfter)
	}
	message := upstreamFailureText
	if code == upstreamErrorContentType {
		message = upstreamContentTypeText
	}
	if code == upstreamErrorNotConfigured {
		message = upstreamNotConfiguredText
	}
	writeJSONError(w, status, code, message)
}

func safeUpstreamCode(code string) string {
	switch code {
	case upstreamErrorBadGateway, upstreamErrorBadRequest, upstreamErrorUnauthorized, upstreamErrorForbidden,
		upstreamErrorRateLimited, upstreamErrorServer, upstreamErrorUnexpected, upstreamErrorContentType,
		upstreamErrorMalformed, upstreamErrorCanceled, upstreamErrorNotConfigured, upstreamErrorNetwork,
		upstreamErrorInvalidRequest, upstreamErrorTooLarge, upstreamErrorUnsupportedTool:
		return code
	default:
		return upstreamErrorBadGateway
	}
}

func closeUpstreamResponse(response *UpstreamResponse) {
	if response == nil || response.Body == nil {
		return
	}
	_ = response.Body.Close()
}

type onceCloser struct {
	once sync.Once
	body io.Closer
}

func (closer *onceCloser) Close() {
	if closer == nil || closer.body == nil {
		return
	}
	closer.once.Do(func() { _ = closer.body.Close() })
}

func watchUpstreamOwnership(stop <-chan struct{}, requestDone <-chan struct{}, sessionDone <-chan struct{}, cancel context.CancelFunc, closeBody func(), finished chan<- struct{}) {
	defer close(finished)
	select {
	case <-stop:
		return
	case <-requestDone:
	case <-sessionDone:
	}
	cancel()
	closeBody()
}
