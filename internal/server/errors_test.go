package server

import "testing"

func TestGatewayErrorTypeCoversStableTaxonomy(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{code: "invalid_request", want: "invalid_request"},
		{code: "feature_not_implemented", want: "unsupported_feature"},
		{code: "request_too_large", want: "request_too_large"},
		{code: "upstream_unauthorized", want: "authentication_error"},
		{code: "permission_error", want: "permission_error"},
		{code: "upstream_rate_limited", want: "rate_limit_error"},
		{code: "upstream_bad_request", want: "provider_bad_request"},
		{code: "upstream_server_error", want: "provider_unavailable"},
		{code: "upstream_stream_error", want: "provider_protocol_error"},
		{code: "stream_interrupted", want: "stream_interrupted"},
		{code: "timeout", want: "timeout"},
		{code: "canceled", want: "canceled"},
		{code: "continuation_unknown", want: "pending_tool_state_not_found"},
		{code: "continuation_expired", want: "pending_tool_state_expired"},
		{code: "internal_error", want: "internal_error"},
		{code: "unsupported_feature", want: "unsupported_feature"},
		{code: "authentication_error", want: "authentication_error"},
		{code: "rate_limit_error", want: "rate_limit_error"},
		{code: "provider_bad_request", want: "provider_bad_request"},
		{code: "provider_unavailable", want: "provider_unavailable"},
		{code: "provider_protocol_error", want: "provider_protocol_error"},
		{code: "pending_tool_state_not_found", want: "pending_tool_state_not_found"},
		{code: "pending_tool_state_expired", want: "pending_tool_state_expired"},
	}
	for _, test := range tests {
		if got := gatewayErrorType(test.code); got != test.want {
			t.Errorf("gatewayErrorType(%q) = %q, want %q", test.code, got, test.want)
		}
	}
}

func TestGatewayErrorTypeDoesNotExposeUnknownCodes(t *testing.T) {
	if got := gatewayErrorType("provider-secret-detail"); got != "internal_error" {
		t.Fatalf("unknown error type = %q, want internal_error", got)
	}
}
