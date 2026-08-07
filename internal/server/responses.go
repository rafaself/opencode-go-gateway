package server

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/codex"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

const (
	featureNotImplementedCode = "feature_not_implemented"
	featureNotImplementedText = "This Responses feature is not implemented in the current milestone."
	requestInvalidText        = "The request could not be processed."
	requestUnsupportedText    = "The request uses an unsupported feature."
	requestTooLargeText       = "The request body exceeds the configured limit."
	upstreamFailureText       = "The upstream provider request failed."
	upstreamContentTypeText   = "The upstream provider returned an unsupported content type."
	upstreamNotConfiguredText = "The upstream provider is not configured."
	requestToolCollisionText  = "The request contains a reserved or conflicting tool name."
)

func (s *Server) handleResponses(w *statusWriter, r *http.Request) {
	if r.Context().Err() != nil {
		return
	}
	if r.Body == nil {
		writeJSONErrorWithParam(w, http.StatusBadRequest, string(codex.ErrorInvalidRequest), "body", requestInvalidText)
		return
	}
	defer r.Body.Close()
	setReadDeadline(w, r.Context(), s.config.RequestBodyReadTimeout)
	defer clearReadDeadline(w)

	decoder, err := codex.NewDecoderWithLimits(codex.DecoderLimits{
		MaxBodyBytes:       s.config.MaxBodyBytes,
		MaxInputItems:      s.config.MaxInputItems,
		MaxCollectionItems: s.config.MaxCollectionItems,
		MaxTools:           s.config.MaxTools,
		MaxSchemaBytes:     s.config.MaxSchemaBytes,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "server_configuration_error", "the gateway is not configured correctly")
		return
	}
	request, err := decoder.DecodeRequest(r)
	if err != nil {
		if requestDecodeWasCanceled(r, err) {
			return
		}
		if isRequestBodyTimeout(err) {
			s.writeJSONErrorAfterBodyTimeout(w, http.StatusRequestTimeout, "timeout", "body", "The request body was not received within the configured phase timeout.")
			return
		}
		writeRequestDecodeError(w, err)
		return
	}
	clearReadDeadline(w)
	if r.Context().Err() != nil {
		return
	}
	registry, err := opencodego.NewToolRegistry(request)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, string(codex.ErrorInvalidRequest), requestToolCollisionText)
		return
	}
	request.ToolRegistry = registry
	results, err := opencodego.TranslateToolResults(request.Input)
	if err != nil {
		switch {
		case errors.Is(err, opencodego.ErrToolResultDuplicate):
			writeContinuationError(w, opencodego.ErrContinuationDuplicate)
		case errors.Is(err, opencodego.ErrToolResultKindMismatch):
			writeContinuationError(w, opencodego.ErrContinuationKindMismatch)
		default:
			writeJSONError(w, http.StatusBadRequest, string(codex.ErrorInvalidRequest), requestInvalidText)
		}
		return
	}
	var lease *opencodego.ContinuationLease
	if len(results) > 0 {
		lease, err = s.continuations.Begin(results)
		if err != nil {
			writeContinuationError(w, err)
			return
		}
		request.Continuation = opencodego.NewContinuationRequest(lease)
		defer func() { _ = lease.Abort() }()
	}
	if err := validateResponsesRequest(request, lease != nil, s.config.MaxTools, s.config.MaxSchemaBytes); err != nil {
		if errors.Is(err, opencodego.ErrContinuationUnknown) {
			writeContinuationError(w, err)
			return
		}
		var decodeError *codex.Error
		if errors.As(err, &decodeError) {
			writeRequestDecodeError(w, err)
			return
		}
		writeJSONError(w, http.StatusNotImplemented, featureNotImplementedCode, err.Error())
		return
	}

	upstreamContext, cancel := context.WithCancel(r.Context())
	defer cancel()
	response, err := s.upstream.Do(upstreamContext, request)
	if err != nil {
		closeUpstreamResponse(response)
		if r.Context().Err() != nil {
			return
		}
		writeUpstreamError(w, err)
		return
	}
	if err := validateUpstreamResponse(response); err != nil {
		closeUpstreamResponse(response)
		if r.Context().Err() != nil {
			return
		}
		writeUpstreamError(w, err)
		return
	}
	bodyCloser := &onceCloser{body: response.Body}
	defer bodyCloser.Close()
	activity := make(chan struct{}, 1)
	streamBody := &activityBody{ReadCloser: response.Body, activity: activity}

	session, err := codex.NewStreamSession(w, codex.StreamSessionOptions{
		// The session must remain writable long enough to emit one safe
		// response.failed for an internal stream timeout. The upstream child
		// context is canceled independently by the watcher; inbound client and
		// shutdown cancellation still cancel this request context immediately.
		Context:                  r.Context(),
		Model:                    opencodego.DefaultModel,
		MaxAggregateBytes:        s.config.MaxSSEBufferedBytes,
		MaxToolCallArgumentBytes: s.config.MaxToolCallArgumentBytes,
		MaxOutputBytes:           s.config.MaxOutputBytes,
		MaxTextBytes:             s.config.MaxTextBytes,
		MaxReasoningBytes:        s.config.MaxReasoningBytes,
		WriteTimeout:             s.config.DownstreamWriteTimeout,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "stream_configuration_error", "the response stream could not be initialized")
		return
	}
	w.responseID = session.ResponseID
	w.responseModel = session.Model
	watcher := newStreamWatcher(r.Context().Done(), session.Done(), cancel, bodyCloser.Close, activity, s.config.StreamIdleTimeout)
	defer watcher.Stop()
	if r.Context().Err() != nil {
		return
	}
	if err := session.Start(); err != nil {
		return
	}

	streamDecoder := opencodego.NewBridgeStreamDecoder(streamBody, opencodego.BridgeStreamDecoderOptions{
		SSE: opencodego.SSEDecoderOptions{
			MaxLineBytes:             s.config.MaxSSELineBytes,
			MaxEventBytes:            s.config.MaxSSEEventBytes,
			MaxBufferedBytes:         s.config.MaxSSEBufferedBytes,
			MaxAggregateBytes:        s.config.MaxSSEBufferedBytes,
			MaxToolCallArgumentBytes: s.config.MaxToolCallArgumentBytes,
			ReadBufferBytes:          s.config.MaxSSEReadBufferBytes,
		},
		AllowedToolNames:       declaredFunctionToolNames(request),
		MaxToolCalls:           s.config.MaxTools,
		MaxProviderCallIDBytes: bridge.DefaultMaxProviderCallIDBytes,
		MaxToolNameBytes:       bridge.DefaultMaxToolNameBytes,
		ToolRegistry:           request.ToolRegistry,
		MaxOutputBytes:         s.config.MaxOutputBytes,
		MaxTextBytes:           s.config.MaxTextBytes,
		MaxReasoningBytes:      s.config.MaxReasoningBytes,
	})
	accepted := lease == nil
	for {
		event, decodeErr := streamDecoder.Next()
		if reason := watcher.Reason(); reason != "" {
			if reason == streamAbortTimeout && !session.TerminalEmitted && session.WriteFailure() == nil && r.Context().Err() == nil {
				_ = session.Fail("timeout", "The upstream response stream exceeded its idle timeout.")
				w.responseTerminal = "response.failed"
			}
			return
		}
		if r.Context().Err() != nil || session.WriteFailure() != nil {
			return
		}
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
		if _, ok := event.(bridge.Completed); ok {
			if r.Context().Err() != nil {
				return
			}
			if turn, hasToolTurn := streamDecoder.PendingTurn(); hasToolTurn {
				if err := s.continuations.SaveContext(r.Context(), *turn); err != nil {
					if r.Context().Err() != nil {
						return
					}
					if !session.TerminalEmitted && session.WriteFailure() == nil {
						_ = session.Fail("continuation_capacity", "The gateway could not retain the provider tool turn.")
						w.responseTerminal = "response.failed"
					}
					return
				}
			}
		}
		if handleErr := session.Handle(event); handleErr != nil {
			if session.TerminalEmitted {
				w.responseTerminal = responseTerminalType(event)
			}
			return
		}
		if !accepted && isContinuationAcceptanceEvent(event) {
			if r.Context().Err() != nil {
				return
			}
			if err := lease.CommitContext(r.Context()); err != nil {
				if r.Context().Err() != nil {
					return
				}
				if !session.TerminalEmitted && session.WriteFailure() == nil {
					_ = session.Fail("continuation_commit", "The gateway could not finalize the continuation.")
					w.responseTerminal = "response.failed"
				}
				return
			}
			accepted = true
		}
		if session.TerminalEmitted {
			w.responseTerminal = responseTerminalType(event)
			return
		}
	}
}

func requestDecodeWasCanceled(request *http.Request, err error) bool {
	if isRequestBodyTimeout(err) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	if request == nil {
		return false
	}
	contextError := request.Context().Err()
	if contextError == nil || errors.Is(contextError, context.DeadlineExceeded) {
		return false
	}
	return !isRequestBodyTimeout(err)
}

// writeJSONErrorAfterBodyTimeout owns the shutdown/write boundary for the
// request. The dedicated lifecycle lock covers only the admission snapshot:
// shutdown either wins before this response is admitted or the timeout write
// is already authorized to finish independently. Neither lifecycle nor
// active-request accounting is held across network I/O. The request context
// itself is never replaced; statusWriter only permits this gateway-owned
// timeout response through its scoped cancellation exception.
func (s *Server) writeJSONErrorAfterBodyTimeout(w *statusWriter, status int, code, param, message string) bool {
	if s == nil || w == nil {
		return false
	}
	s.lifecycleMu.Lock()
	s.activeRequestsMu.Lock()
	shuttingDown := s.shuttingDown
	if !shuttingDown {
		w.allowCanceledWrite = true
	}
	s.activeRequestsMu.Unlock()
	s.lifecycleMu.Unlock()
	if shuttingDown {
		return false
	}
	defer func() { w.allowCanceledWrite = false }()
	setWriteDeadline(w, s.config.DownstreamWriteTimeout)
	defer clearWriteDeadline(w)
	writeJSONErrorWithParam(w, status, code, param, message)
	return w.status == status
}

func isRequestBodyTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func isContinuationAcceptanceEvent(event bridge.StreamEvent) bool {
	return event != nil && event.StreamEventKind() == bridge.StreamResponseStarted
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

func validateResponsesRequest(request bridge.Request, continuation bool, maxProviderTools int, maxSchemaBytes int64) error {
	if request.PreviousResponseID != "" && !continuation {
		return opencodego.ErrContinuationUnknown
	}
	if err := opencodego.ValidateProviderToolBudget(request, maxProviderTools, maxSchemaBytes); err != nil {
		var providerError *opencodego.ProviderError
		if errors.As(err, &providerError) && providerError.Code == opencodego.ErrorRequestTooLarge {
			return &codex.Error{Code: codex.ErrorRequestTooLarge, Param: "tools", Message: requestTooLargeText}
		}
		return err
	}
	for _, tool := range request.Tools {
		if tool == nil {
			return errors.New(featureNotImplementedText)
		}
		switch tool := tool.(type) {
		case bridge.FunctionTool:
		case bridge.CustomTool:
			if tool.Name != opencodego.ApplyPatchToolName || (tool.Format.Kind != "" && tool.Format.Kind != bridge.CustomToolFormatText && tool.Format.Kind != bridge.CustomToolFormatGrammar) {
				return &codex.Error{Code: codex.ErrorInvalidRequest, Param: "tools", Message: "only the apply_patch custom tool is supported"}
			}
		case bridge.DeferredTool:
			if err := opencodego.ValidateCapturedDeferredTool(tool); err != nil {
				return err
			}
		default:
			return errors.New(featureNotImplementedText)
		}
	}
	for _, item := range request.Input {
		if item == nil {
			return errors.New(featureNotImplementedText)
		}
		switch item := item.(type) {
		case bridge.Message:
		case bridge.FunctionCall, bridge.FunctionCallOutput:
			if !continuation {
				return errors.New("function tool continuation is not available")
			}
		case bridge.CustomToolCall:
			if !continuation {
				return errors.New("custom tool continuation is not available")
			}
			if item.Name != opencodego.ApplyPatchToolName {
				return &codex.Error{Code: codex.ErrorInvalidRequest, Param: "input", Message: "only apply_patch custom tool calls are supported"}
			}
		case bridge.CustomToolCallOutput:
			if !continuation {
				return errors.New("custom tool continuation is not available")
			}
		default:
			return errors.New("function tool results and prior function calls are not implemented in the current milestone")
		}
	}
	switch request.ToolChoice.Kind {
	case bridge.ToolChoiceUnset, bridge.ToolChoiceAuto, bridge.ToolChoiceNone:
	case bridge.ToolChoiceRequired, bridge.ToolChoiceFunction:
		return &codex.Error{Code: codex.ErrorInvalidRequest, Param: "tool_choice", Message: "forced and named tool choices are not supported"}
	default:
		return &codex.Error{Code: codex.ErrorInvalidRequest, Param: "tool_choice", Message: "tool choice is not supported"}
	}
	if format := request.Generation.Text.Format.Kind; format != "" && format != bridge.TextFormatText {
		return errors.New("structured response formats are not implemented in the current milestone")
	}
	return nil
}

func writeContinuationError(w *statusWriter, err error) {
	code := "continuation_invalid"
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, opencodego.ErrContinuationUnknown):
		code = "continuation_unknown"
	case errors.Is(err, opencodego.ErrContinuationExpired):
		code = "continuation_expired"
	case errors.Is(err, opencodego.ErrContinuationBusy):
		code = "continuation_busy"
		status = http.StatusConflict
	case errors.Is(err, opencodego.ErrContinuationConsumed):
		code = "continuation_consumed"
	case errors.Is(err, opencodego.ErrContinuationIncomplete):
		code = "continuation_incomplete"
	case errors.Is(err, opencodego.ErrContinuationKindMismatch):
		code = "continuation_kind_mismatch"
	case errors.Is(err, opencodego.ErrContinuationMixed):
		code = "continuation_mixed"
	case errors.Is(err, opencodego.ErrContinuationDuplicate):
		code = "continuation_duplicate"
	case errors.Is(err, opencodego.ErrContinuationCapacity):
		code = "continuation_capacity"
		status = http.StatusServiceUnavailable
	case errors.Is(err, opencodego.ErrContinuationClosed):
		code = "continuation_closed"
		status = http.StatusServiceUnavailable
	}
	writeJSONError(w, status, code, requestInvalidText)
}

func declaredFunctionToolNames(request bridge.Request) []string {
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		function, ok := tool.(bridge.FunctionTool)
		if ok {
			names = append(names, function.Name)
		}
	}
	return names
}

func writeRequestDecodeError(w *statusWriter, err error) {
	var decodeError *codex.Error
	if !errors.As(err, &decodeError) {
		writeJSONErrorWithParam(w, http.StatusBadRequest, string(codex.ErrorInvalidRequest), "body", requestInvalidText)
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
	writeJSONErrorWithParam(w, status, string(decodeError.Code), decodeError.Param, message)
}

func validateUpstreamResponse(response *UpstreamResponse) error {
	if response == nil {
		return &UpstreamError{Code: upstreamErrorBadGateway}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &UpstreamError{Code: upstreamCodeForStatus(response.StatusCode), StatusCode: response.StatusCode, RetryAfter: safeRetryAfterHeader(response.Header.Get("Retry-After"))}
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
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return upstreamErrorTimeout
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
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
		if code == upstreamErrorTimeout {
			status = http.StatusGatewayTimeout
		}
		if code == upstreamErrorBadRequest || code == upstreamErrorInvalidRequest || code == upstreamErrorTooLarge || code == upstreamErrorUnsupportedTool {
			status = http.StatusBadRequest
		}
		if code == upstreamErrorNotConfigured {
			status = http.StatusInternalServerError
		}
	} else {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			code = upstreamErrorTimeout
			status = http.StatusGatewayTimeout
		case errors.Is(err, context.Canceled):
			code = upstreamErrorCanceled
		case isNetworkTimeout(err):
			code = upstreamErrorTimeout
			status = http.StatusGatewayTimeout
		case err != nil:
			code = upstreamErrorNetwork
		}
	}
	if code == upstreamErrorRateLimited {
		if retryAfter = safeRetryAfterHeader(retryAfter); retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
	}
	message := upstreamFailureText
	if code == upstreamErrorContentType {
		message = upstreamContentTypeText
	}
	if code == upstreamErrorNotConfigured {
		message = upstreamNotConfiguredText
	}
	if code == upstreamErrorTimeout {
		message = "The upstream provider did not respond within the configured phase timeout."
	}
	if code == upstreamErrorCanceled {
		message = "The upstream request was canceled."
	}
	writeJSONError(w, status, code, message)
}

func isNetworkTimeout(err error) bool {
	var networkErr net.Error
	return errors.As(err, &networkErr) && networkErr.Timeout()
}

func safeRetryAfterHeader(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	if allDecimalDigits(value) {
		return value
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(http.TimeFormat)
}

func allDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, runeValue := range value {
		if runeValue < '0' || runeValue > '9' {
			return false
		}
	}
	return true
}

func safeUpstreamCode(code string) string {
	switch code {
	case upstreamErrorBadGateway, upstreamErrorBadRequest, upstreamErrorUnauthorized, upstreamErrorForbidden,
		upstreamErrorRateLimited, upstreamErrorServer, upstreamErrorUnexpected, upstreamErrorContentType,
		upstreamErrorMalformed, upstreamErrorCanceled, upstreamErrorTimeout, upstreamErrorNotConfigured, upstreamErrorNetwork,
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

const (
	streamAbortCanceled = "canceled"
	streamAbortTimeout  = "timeout"
	streamAbortWrite    = "stream_interrupted"
)

type activityBody struct {
	io.ReadCloser
	activity chan<- struct{}
}

func (body *activityBody) Read(target []byte) (int, error) {
	if body == nil || body.ReadCloser == nil {
		return 0, io.EOF
	}
	n, err := body.ReadCloser.Read(target)
	if n > 0 && body.activity != nil {
		select {
		case body.activity <- struct{}{}:
		default:
		}
	}
	return n, err
}

type streamWatcher struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	reason   string
}

func newStreamWatcher(requestDone <-chan struct{}, sessionDone <-chan struct{}, cancel context.CancelFunc, closeBody func(), activity <-chan struct{}, idle time.Duration) *streamWatcher {
	watcher := &streamWatcher{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go watcher.run(requestDone, sessionDone, cancel, closeBody, activity, idle)
	return watcher
}

func (watcher *streamWatcher) run(requestDone <-chan struct{}, sessionDone <-chan struct{}, cancel context.CancelFunc, closeBody func(), activity <-chan struct{}, idle time.Duration) {
	defer close(watcher.done)
	if idle <= 0 {
		idle = defaultStreamIdleTimeout
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-watcher.stop:
			return
		case <-requestDone:
			watcher.interrupt(streamAbortCanceled, cancel, closeBody)
			return
		case <-sessionDone:
			watcher.interrupt(streamAbortWrite, cancel, closeBody)
			return
		case <-activity:
			resetTimer(timer, idle)
		case <-timer.C:
			watcher.interrupt(streamAbortTimeout, cancel, closeBody)
			return
		}
	}
}

func (watcher *streamWatcher) interrupt(reason string, cancel context.CancelFunc, closeBody func()) {
	watcher.mu.Lock()
	if watcher.reason == "" {
		watcher.reason = reason
	}
	watcher.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if closeBody != nil {
		closeBody()
	}
}

func (watcher *streamWatcher) Reason() string {
	if watcher == nil {
		return ""
	}
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	return watcher.reason
}

func (watcher *streamWatcher) Stop() {
	if watcher == nil {
		return
	}
	watcher.stopOnce.Do(func() { close(watcher.stop) })
	<-watcher.done
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}
