package server

// The public error type is deliberately smaller than provider-specific error
// codes. The code remains a stable diagnostic detail for existing clients,
// while type gives callers one deterministic taxonomy across providers.
const (
	gatewayPermissionCode  = "permission_error"
	gatewayActiveLimitCode = "active_request_limit"
	gatewayShutdownCode    = "server_shutting_down"
)

func gatewayErrorType(code string) string {
	switch code {
	case "invalid_request", "malformed_json", "continuation_invalid", "continuation_busy", "continuation_consumed", "continuation_incomplete", "continuation_kind_mismatch", "continuation_mixed", "continuation_duplicate", "method_not_allowed", "not_found":
		return "invalid_request"
	case "request_too_large", "stream_limit_exceeded":
		return "request_too_large"
	case "feature_not_implemented", "unsupported_field", "unsupported_item_type", "unsupported_tool_type", "unsupported_feature":
		return "unsupported_feature"
	case "upstream_unauthorized", "authentication_error":
		return "authentication_error"
	case "upstream_forbidden", gatewayPermissionCode:
		return "permission_error"
	case "upstream_rate_limited", gatewayActiveLimitCode, "rate_limit_error":
		return "rate_limit_error"
	case "upstream_bad_request", "upstream_invalid_request", "upstream_request_too_large", "upstream_unsupported_tool", "provider_bad_request":
		return "provider_bad_request"
	case "upstream_server_error", "upstream_network_error", "upstream_not_configured", "provider_unavailable", gatewayShutdownCode:
		return "provider_unavailable"
	case "upstream_error", "upstream_unexpected_status", "upstream_unsupported_content_type", "upstream_malformed_response", "upstream_stream_error", "upstream_eof", "upstream_terminal_error", "upstream_tool_not_declared", "upstream_custom_tool_invalid", "provider_protocol_error":
		return "provider_protocol_error"
	case "upstream_timeout", "timeout":
		return "timeout"
	case "upstream_canceled", "canceled":
		return "canceled"
	case "continuation_unknown", "pending_tool_state_not_found":
		return "pending_tool_state_not_found"
	case "continuation_expired", "pending_tool_state_expired":
		return "pending_tool_state_expired"
	case "stream_interrupted":
		return "stream_interrupted"
	case "server_configuration_error", "stream_configuration_error", "continuation_capacity", "continuation_closed", "internal_error":
		return "internal_error"
	default:
		return "internal_error"
	}
}
