package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

func TestLoadUsesTypedDefaultsAndDoesNotExposeAPIKey(t *testing.T) {
	apiKey := "sk-test-config-secret"
	config, err := Load(lookupEnv(map[string]string{
		"OPENCODE_GO_API_KEY": apiKey,
	}))
	if err != nil {
		t.Fatal(err)
	}

	if config.BaseURL != DefaultBaseURL {
		t.Fatalf("base URL = %q, want %q", config.BaseURL, DefaultBaseURL)
	}
	if config.ZenBaseURL != DefaultZenBaseURL {
		t.Fatalf("zen base URL = %q, want %q", config.ZenBaseURL, DefaultZenBaseURL)
	}
	if config.Model != opencodego.DefaultModel {
		t.Fatalf("model = %q, want %q", config.Model, opencodego.DefaultModel)
	}
	if config.ZenModel != opencodego.DeepSeekV4FlashFreeModel {
		t.Fatalf("zen model = %q, want %q", config.ZenModel, opencodego.DeepSeekV4FlashFreeModel)
	}
	if config.Host != DefaultHost {
		t.Fatalf("host = %q, want %q", config.Host, DefaultHost)
	}
	if config.Port != DefaultPort {
		t.Fatalf("port = %d, want %d", config.Port, DefaultPort)
	}
	if config.ShutdownTimeout != DefaultShutdownTimeout {
		t.Fatalf("shutdown timeout = %s, want %s", config.ShutdownTimeout, DefaultShutdownTimeout)
	}
	if config.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("max body bytes = %d, want %d", config.MaxBodyBytes, DefaultMaxBodyBytes)
	}

	formatted := fmt.Sprintf("%+v", config)
	if strings.Contains(formatted, apiKey) {
		t.Fatalf("formatted config contains API key: %s", formatted)
	}
	goSyntax := fmt.Sprintf("%#v", config)
	if strings.Contains(goSyntax, apiKey) {
		t.Fatalf("Go-syntax config contains API key: %s", goSyntax)
	}
	if !strings.Contains(formatted, "api_key=<redacted>") {
		t.Fatalf("formatted config does not advertise redaction: %s", formatted)
	}
}

func TestLoadParsesOperationalSettingsWithoutProcessEnvironment(t *testing.T) {
	config, err := Load(lookupEnv(map[string]string{
		"OPENCODE_GO_API_KEY":                           "test-key",
		"OPENCODE_GO_BASE_URL":                          "https://provider.example/v1",
		"OPENCODE_GO_ZEN_BASE_URL":                      "https://zen.example/v2",
		"OPENCODE_GO_ZEN_MODEL":                         "deepseek-v4-pro",
		"OPENCODE_GATEWAY_HOST":                         "127.0.0.1",
		"OPENCODE_GATEWAY_PORT":                         "9090",
		"OPENCODE_GATEWAY_LOG_LEVEL":                    "debug",
		"OPENCODE_GATEWAY_SHUTDOWN_TIMEOUT":             "3s",
		"OPENCODE_GATEWAY_READ_HEADER_TIMEOUT":          "250ms",
		"OPENCODE_GATEWAY_IDLE_TIMEOUT":                 "6s",
		"OPENCODE_GATEWAY_REQUEST_BODY_READ_TIMEOUT":    "7s",
		"OPENCODE_GATEWAY_UPSTREAM_CONNECT_TIMEOUT":     "8s",
		"OPENCODE_GATEWAY_TLS_HANDSHAKE_TIMEOUT":        "9s",
		"OPENCODE_GATEWAY_RESPONSE_HEADER_TIMEOUT":      "10s",
		"OPENCODE_GATEWAY_STREAM_IDLE_TIMEOUT":          "11s",
		"OPENCODE_GATEWAY_DOWNSTREAM_WRITE_TIMEOUT":     "12s",
		"OPENCODE_GATEWAY_MAX_BODY_BYTES":               "4096",
		"OPENCODE_GATEWAY_MAX_HEADER_BYTES":             "8192",
		"OPENCODE_GATEWAY_MAX_INPUT_ITEMS":              "3",
		"OPENCODE_GATEWAY_MAX_COLLECTION_ITEMS":         "4",
		"OPENCODE_GATEWAY_MAX_TOOLS":                    "4",
		"OPENCODE_GATEWAY_MAX_SCHEMA_BYTES":             "5",
		"OPENCODE_GATEWAY_MAX_SSE_LINE_BYTES":           "6",
		"OPENCODE_GATEWAY_MAX_SSE_EVENT_BYTES":          "7",
		"OPENCODE_GATEWAY_MAX_SSE_BUFFERED_BYTES":       "8",
		"OPENCODE_GATEWAY_MAX_SSE_READ_BUFFER_BYTES":    "9",
		"OPENCODE_GATEWAY_MAX_OUTPUT_BYTES":             "10",
		"OPENCODE_GATEWAY_MAX_TEXT_BYTES":               "11",
		"OPENCODE_GATEWAY_MAX_REASONING_BYTES":          "12",
		"OPENCODE_GATEWAY_MAX_TOOL_CALL_ARGUMENT_BYTES": "13",
		"OPENCODE_GATEWAY_MAX_PENDING_TURN_BYTES":       "14",
		"OPENCODE_GATEWAY_MAX_PENDING_RECORDS":          "16",
		"OPENCODE_GATEWAY_MAX_PENDING_AGGREGATE_BYTES":  "17",
		"OPENCODE_GATEWAY_MAX_ACTIVE_REQUESTS":          "15",
	}))
	if err != nil {
		t.Fatal(err)
	}

	if config.BaseURL != "https://provider.example/v1" || config.Port != 9090 || config.LogLevel.String() != "DEBUG" {
		t.Fatalf("parsed config = %+v", config)
	}
	if config.ZenBaseURL != "https://zen.example/v2" || config.ZenModel != "deepseek-v4-pro" {
		t.Fatalf("parsed zen settings = %+v", config)
	}
	if config.ShutdownTimeout != 3*time.Second || config.ReadHeaderTimeout != 250*time.Millisecond || config.IdleTimeout != 6*time.Second {
		t.Fatalf("parsed timeouts = %+v", config)
	}
	if config.RequestBodyReadTimeout != 7*time.Second || config.UpstreamConnectTimeout != 8*time.Second || config.TLSHandshakeTimeout != 9*time.Second || config.ResponseHeaderTimeout != 10*time.Second || config.StreamIdleTimeout != 11*time.Second || config.DownstreamWriteTimeout != 12*time.Second {
		t.Fatalf("parsed phase timeouts = %+v", config)
	}
	if config.MaxBodyBytes != 4096 || config.MaxHeaderBytes != 8192 {
		t.Fatalf("parsed limits = %+v", config)
	}
	if config.MaxInputItems != 3 || config.MaxCollectionItems != 4 || config.MaxTools != 4 || config.MaxSchemaBytes != 5 || config.MaxSSELineBytes != 6 || config.MaxSSEEventBytes != 7 || config.MaxSSEBufferedBytes != 8 || config.MaxSSEReadBufferBytes != 9 || config.MaxOutputBytes != 10 || config.MaxTextBytes != 11 || config.MaxReasoningBytes != 12 || config.MaxToolCallArgumentBytes != 13 || config.MaxPendingTurnBytes != 14 || config.MaxPendingRecords != 16 || config.MaxPendingAggregateBytes != 17 || config.MaxActiveRequests != 15 {
		t.Fatalf("parsed resource limits = %+v", config)
	}
}

func TestLoadRejectsNonPositivePhaseAndResourceLimits(t *testing.T) {
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "stream timeout", key: "OPENCODE_GATEWAY_STREAM_IDLE_TIMEOUT"},
		{name: "input limit", key: "OPENCODE_GATEWAY_MAX_INPUT_ITEMS"},
		{name: "pending limit", key: "OPENCODE_GATEWAY_MAX_PENDING_TURN_BYTES"},
		{name: "collection limit", key: "OPENCODE_GATEWAY_MAX_COLLECTION_ITEMS"},
		{name: "pending record limit", key: "OPENCODE_GATEWAY_MAX_PENDING_RECORDS"},
		{name: "pending aggregate limit", key: "OPENCODE_GATEWAY_MAX_PENDING_AGGREGATE_BYTES"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{
				"OPENCODE_GO_API_KEY": "test-key",
				test.key:              "0",
			}
			_, err := Load(lookupEnv(values))
			if err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("error = %v, want validation for %s", err, test.key)
			}
		})
	}
}

func TestValidateRejectsProviderMapperBudgetOverflow(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*Config)
		want string
	}{
		{
			name: "provider tool slots",
			set: func(config *Config) {
				config.MaxTools = bridge.DefaultMaxProviderTools + 1
			},
			want: "OPENCODE_GATEWAY_MAX_TOOLS",
		},
		{
			name: "provider schema bytes",
			set: func(config *Config) {
				config.MaxSchemaBytes = int64(bridge.DefaultMaxFunctionSchemaBytes) + 1
			},
			want: "OPENCODE_GATEWAY_MAX_SCHEMA_BYTES",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Defaults().WithAPIKey("test-key")
			test.set(&config)
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want deterministic %s rejection", err, test.want)
			}
		})
	}
}

func TestDefaultsUseSharedProviderBudgetAndKeep127ToolBoundary(t *testing.T) {
	config := Defaults()
	if config.MaxTools != bridge.DefaultMaxProviderTools {
		t.Fatalf("default tool budget = %d, want %d", config.MaxTools, bridge.DefaultMaxProviderTools)
	}
	if config.MaxSchemaBytes != int64(bridge.DefaultMaxFunctionSchemaBytes) {
		t.Fatalf("default schema budget = %d, want %d", config.MaxSchemaBytes, bridge.DefaultMaxFunctionSchemaBytes)
	}
	if config.MaxTools-1 != 127 {
		t.Fatalf("implicit apply_patch boundary = %d, want 127 ordinary tools", config.MaxTools-1)
	}
	if err := config.WithAPIKey("test-key").Validate(); err != nil {
		t.Fatalf("default configuration validation = %v", err)
	}
}

func TestLoadRejectsMissingAPIKeyWithoutEchoingSecrets(t *testing.T) {
	_, err := Load(lookupEnv(map[string]string{
		"OPENCODE_GATEWAY_HOST": "127.0.0.1",
	}))
	if err == nil {
		t.Fatal("missing API key was accepted")
	}
	if got := err.Error(); got != "OPENCODE_GO_API_KEY is required" {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadRejectsNonLoopbackUnlessExplicitlyAllowed(t *testing.T) {
	_, err := Load(lookupEnv(map[string]string{
		"OPENCODE_GO_API_KEY":   "test-key",
		"OPENCODE_GATEWAY_HOST": "0.0.0.0",
	}))
	if err == nil {
		t.Fatal("non-loopback binding was accepted without opt-in")
	}

	config, err := Load(lookupEnv(map[string]string{
		"OPENCODE_GO_API_KEY":                 "test-key",
		"OPENCODE_GATEWAY_HOST":               "0.0.0.0",
		"OPENCODE_GATEWAY_ALLOW_NON_LOOPBACK": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !config.AllowNonLoopback {
		t.Fatal("explicit non-loopback opt-in was not retained")
	}
}

func TestLoadRejectsInvalidZenSettings(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{
			name: "zen base URL scheme",
			values: map[string]string{
				"OPENCODE_GO_API_KEY":      "test-key",
				"OPENCODE_GO_ZEN_BASE_URL": "http://zen.example/v1",
			},
			want: "OPENCODE_GO_ZEN_BASE_URL",
		},
		{
			name: "zen base URL credentials",
			values: map[string]string{
				"OPENCODE_GO_API_KEY":      "test-key",
				"OPENCODE_GO_ZEN_BASE_URL": "https://user:secret@zen.example/v1",
			},
			want: "OPENCODE_GO_ZEN_BASE_URL",
		},
		{
			name: "zen model unsupported",
			values: map[string]string{
				"OPENCODE_GO_API_KEY":   "test-key",
				"OPENCODE_GO_ZEN_MODEL": "gpt-5",
			},
			want: "OPENCODE_GO_ZEN_MODEL",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(lookupEnv(test.values))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %s rejection", err, test.want)
			}
		})
	}
}

func TestLoadRejectsBaseURLCredentialsQueriesAndTraversal(t *testing.T) {
	for _, baseURL := range []string{
		"https://user:secret@provider.example/v1",
		"https://provider.example/v1?token=secret",
		"https://provider.example/v1#secret",
		"https://provider.example/v1/../private",
	} {
		_, err := Load(lookupEnv(map[string]string{
			"OPENCODE_GO_API_KEY":  "test-key",
			"OPENCODE_GO_BASE_URL": baseURL,
		}))
		if err == nil {
			t.Fatalf("base URL %q was accepted", baseURL)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("base URL validation exposed a secret: %v", err)
		}
	}
}

func lookupEnv(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
