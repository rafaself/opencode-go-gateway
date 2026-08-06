package opencodego

import "fmt"

// ErrorCode is the stable category of a provider-client failure. The error
// deliberately does not retain or expose the upstream error body.
type ErrorCode string

const (
	ErrorInvalidConfiguration      ErrorCode = "invalid_configuration"
	ErrorInvalidRequest            ErrorCode = "invalid_request"
	ErrorRequestTooLarge           ErrorCode = "request_too_large"
	ErrorUnsupportedTool           ErrorCode = "unsupported_tool"
	ErrorUnsupportedToolChoice     ErrorCode = "unsupported_tool_choice"
	ErrorUnsupportedResponseFormat ErrorCode = "unsupported_response_format"
	ErrorNetwork                   ErrorCode = "network_error"
	ErrorCanceled                  ErrorCode = "canceled"
	ErrorTimeout                   ErrorCode = "timeout"
	ErrorBadRequest                ErrorCode = "upstream_bad_request"
	ErrorUnauthorized              ErrorCode = "upstream_unauthorized"
	ErrorForbidden                 ErrorCode = "upstream_forbidden"
	ErrorRateLimited               ErrorCode = "upstream_rate_limited"
	ErrorServer                    ErrorCode = "upstream_server_error"
	ErrorUnexpectedStatus          ErrorCode = "upstream_unexpected_status"
	ErrorUnsupportedContentType    ErrorCode = "unsupported_content_type"
	ErrorMalformedResponse         ErrorCode = "malformed_response"
)

func (code ErrorCode) String() string { return string(code) }

// ProviderError is returned for configuration, mapping, transport, and
// upstream response failures. StatusCode and RetryAfter are safe response
// metadata; provider response bodies are intentionally not retained.
type ProviderError struct {
	Code        ErrorCode
	StatusCode  int
	RetryAfter  string
	ContentType string
	cause       error
}

func (err *ProviderError) Error() string {
	if err == nil {
		return "<nil>"
	}
	if err.StatusCode != 0 {
		return fmt.Sprintf("opencode provider error: %s (status %d)", err.Code, err.StatusCode)
	}
	return "opencode provider error: " + string(err.Code)
}

// Unwrap preserves context cancellation and network identity for callers
// without placing the underlying error text in the public message.
func (err *ProviderError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func providerError(code ErrorCode, cause error) *ProviderError {
	return &ProviderError{Code: code, cause: cause}
}
