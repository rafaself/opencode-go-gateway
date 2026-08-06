package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
		"OPENCODE_GO_API_KEY":                  "test-key",
		"OPENCODE_GO_BASE_URL":                 "https://provider.example/v1",
		"OPENCODE_GATEWAY_HOST":                "127.0.0.1",
		"OPENCODE_GATEWAY_PORT":                "9090",
		"OPENCODE_GATEWAY_LOG_LEVEL":           "debug",
		"OPENCODE_GATEWAY_SHUTDOWN_TIMEOUT":    "3s",
		"OPENCODE_GATEWAY_READ_HEADER_TIMEOUT": "250ms",
		"OPENCODE_GATEWAY_READ_TIMEOUT":        "4s",
		"OPENCODE_GATEWAY_WRITE_TIMEOUT":       "5s",
		"OPENCODE_GATEWAY_IDLE_TIMEOUT":        "6s",
		"OPENCODE_GATEWAY_MAX_BODY_BYTES":      "4096",
		"OPENCODE_GATEWAY_MAX_HEADER_BYTES":    "8192",
	}))
	if err != nil {
		t.Fatal(err)
	}

	if config.BaseURL != "https://provider.example/v1" || config.Port != 9090 || config.LogLevel.String() != "DEBUG" {
		t.Fatalf("parsed config = %+v", config)
	}
	if config.ShutdownTimeout != 3*time.Second || config.ReadHeaderTimeout != 250*time.Millisecond || config.ReadTimeout != 4*time.Second || config.WriteTimeout != 5*time.Second || config.IdleTimeout != 6*time.Second {
		t.Fatalf("parsed timeouts = %+v", config)
	}
	if config.MaxBodyBytes != 4096 || config.MaxHeaderBytes != 8192 {
		t.Fatalf("parsed limits = %+v", config)
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

func lookupEnv(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
