package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

const (
	upstreamErrorBadGateway      = "upstream_error"
	upstreamErrorBadRequest      = "upstream_bad_request"
	upstreamErrorUnauthorized    = "upstream_unauthorized"
	upstreamErrorForbidden       = "upstream_forbidden"
	upstreamErrorRateLimited     = "upstream_rate_limited"
	upstreamErrorServer          = "upstream_server_error"
	upstreamErrorUnexpected      = "upstream_unexpected_status"
	upstreamErrorContentType     = "upstream_unsupported_content_type"
	upstreamErrorMalformed       = "upstream_malformed_response"
	upstreamErrorCanceled        = "upstream_canceled"
	upstreamErrorTimeout         = "upstream_timeout"
	upstreamErrorNotConfigured   = "upstream_not_configured"
	upstreamErrorNetwork         = "upstream_network_error"
	upstreamErrorInvalidRequest  = "upstream_invalid_request"
	upstreamErrorTooLarge        = "upstream_request_too_large"
	upstreamErrorUnsupportedTool = "upstream_unsupported_tool"
)

// UpstreamResponse is the server-owned part of a successful provider
// response. The handler owns Body and closes it on every exit path.
type UpstreamResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// UpstreamClient is the narrow orchestration seam for the configured
// provider. It carries bridge semantics in and a response body out, keeping
// provider wire types behind the server adapter.
type UpstreamClient interface {
	Do(context.Context, bridge.Request) (*UpstreamResponse, error)
}

// UpstreamClientFunc adapts a function into an UpstreamClient. It is useful
// for deterministic integration tests without coupling them to provider wire
// structs.
type UpstreamClientFunc func(context.Context, bridge.Request) (*UpstreamResponse, error)

func (client UpstreamClientFunc) Do(ctx context.Context, request bridge.Request) (*UpstreamResponse, error) {
	return client(ctx, request)
}

// UpstreamError is a stable, provider-neutral pre-stream failure. Its Error
// method deliberately omits provider payloads, credentials, and causes.
type UpstreamError struct {
	Code       string
	StatusCode int
	RetryAfter string
}

func (err *UpstreamError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return "upstream request failed"
}

type openCodeDoer interface {
	Do(context.Context, bridge.Request) (*opencodego.Response, error)
}

type openCodeUpstreamClient struct {
	client openCodeDoer
}

// NewOpenCodeUpstreamClient hides internal/opencodego response types at the
// server boundary while allowing the application to inject the real client.
func NewOpenCodeUpstreamClient(client openCodeDoer) UpstreamClient {
	return openCodeUpstreamClient{client: client}
}

func (client openCodeUpstreamClient) Do(ctx context.Context, request bridge.Request) (*UpstreamResponse, error) {
	if client.client == nil {
		return nil, &UpstreamError{Code: upstreamErrorNotConfigured}
	}
	response, err := client.client.Do(ctx, request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, normalizeProviderError(err)
	}
	if response == nil {
		return nil, &UpstreamError{Code: upstreamErrorBadGateway}
	}
	return &UpstreamResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
		Body:       response.Body,
	}, nil
}

func normalizeProviderError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &UpstreamError{Code: upstreamErrorTimeout}
	}
	if errors.Is(err, context.Canceled) {
		return &UpstreamError{Code: upstreamErrorCanceled}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return &UpstreamError{Code: upstreamErrorTimeout}
	}
	var providerErr *opencodego.ProviderError
	if !errors.As(err, &providerErr) {
		return &UpstreamError{Code: upstreamErrorNetwork}
	}
	code := upstreamErrorBadGateway
	switch providerErr.Code {
	case opencodego.ErrorInvalidRequest:
		code = upstreamErrorInvalidRequest
	case opencodego.ErrorRequestTooLarge:
		code = upstreamErrorTooLarge
	case opencodego.ErrorUnsupportedTool, opencodego.ErrorUnsupportedToolChoice:
		code = upstreamErrorUnsupportedTool
	case opencodego.ErrorCanceled:
		code = upstreamErrorCanceled
	case opencodego.ErrorTimeout:
		code = upstreamErrorTimeout
	case opencodego.ErrorBadRequest:
		code = upstreamErrorBadRequest
	case opencodego.ErrorUnauthorized:
		code = upstreamErrorUnauthorized
	case opencodego.ErrorForbidden:
		code = upstreamErrorForbidden
	case opencodego.ErrorRateLimited:
		code = upstreamErrorRateLimited
	case opencodego.ErrorServer:
		code = upstreamErrorServer
	case opencodego.ErrorUnexpectedStatus:
		code = upstreamErrorUnexpected
	case opencodego.ErrorUnsupportedContentType:
		code = upstreamErrorContentType
	case opencodego.ErrorMalformedResponse:
		code = upstreamErrorMalformed
	case opencodego.ErrorNetwork:
		code = upstreamErrorNetwork
	}
	return &UpstreamError{
		Code:       code,
		StatusCode: providerErr.StatusCode,
		RetryAfter: providerErr.RetryAfter,
	}
}
