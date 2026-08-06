package codex

import "fmt"

// ErrorCode is the stable machine-readable category returned by the Codex
// request boundary.
type ErrorCode string

const (
	ErrorInvalidRequest      ErrorCode = "invalid_request"
	ErrorMalformedJSON       ErrorCode = "malformed_json"
	ErrorRequestTooLarge     ErrorCode = "request_too_large"
	ErrorUnsupportedField    ErrorCode = "unsupported_field"
	ErrorUnsupportedItemType ErrorCode = "unsupported_item_type"
	ErrorUnsupportedToolType ErrorCode = "unsupported_tool_type"
)

// Error identifies one request-boundary failure without exposing raw request
// content. Code and Param are suitable for stable HTTP mapping in a later
// server milestone; Message is intended for operators and clients.
type Error struct {
	Code    ErrorCode `json:"code"`
	Param   string    `json:"param"`
	Message string    `json:"message"`
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	if err.Param == "" {
		return fmt.Sprintf("%s: %s", err.Code, err.Message)
	}
	return fmt.Sprintf("%s (%s): %s", err.Code, err.Param, err.Message)
}

func newError(code ErrorCode, param, message string) *Error {
	return &Error{Code: code, Param: param, Message: message}
}

func invalidRequest(param, message string) *Error {
	return newError(ErrorInvalidRequest, param, message)
}

func unsupportedField(param, message string) *Error {
	return newError(ErrorUnsupportedField, param, message)
}
