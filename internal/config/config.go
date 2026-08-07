package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
)

const (
	DefaultBaseURL = "https://opencode.ai/zen/go/v1"
	DefaultHost    = "127.0.0.1"
	DefaultPort    = 8787

	DefaultShutdownTimeout          = 10 * time.Second
	DefaultReadHeaderTimeout        = 5 * time.Second
	DefaultRequestBodyReadTimeout   = 30 * time.Second
	DefaultDownstreamWriteTimeout   = 30 * time.Second
	DefaultIdleTimeout              = 60 * time.Second
	DefaultUpstreamConnectTimeout   = 10 * time.Second
	DefaultTLSHandshakeTimeout      = 10 * time.Second
	DefaultResponseHeaderTimeout    = 30 * time.Second
	DefaultStreamIdleTimeout        = 60 * time.Second
	DefaultMaxBodyBytes             = int64(16 << 20)
	DefaultMaxHeaderBytes           = 64 << 10
	DefaultMaxInputItems            = 256
	DefaultMaxCollectionItems       = bridge.DefaultMaxCollectionItems
	DefaultMaxTools                 = bridge.DefaultMaxProviderTools
	DefaultMaxSchemaBytes           = int64(bridge.DefaultMaxFunctionSchemaBytes)
	DefaultMaxSSELineBytes          = 256 << 10
	DefaultMaxSSEEventBytes         = 4 << 20
	DefaultMaxSSEBufferedBytes      = 8 << 20
	DefaultMaxSSEReadBufferBytes    = 32 << 10
	DefaultMaxOutputBytes           = 16 << 20
	DefaultMaxTextBytes             = 8 << 20
	DefaultMaxReasoningBytes        = 8 << 20
	DefaultMaxToolCallArgumentBytes = 1 << 20
	DefaultMaxPendingTurnBytes      = int64(16 << 20)
	DefaultMaxPendingRecords        = 128
	DefaultMaxPendingAggregateBytes = int64(128 << 20)
	DefaultMaxActiveRequests        = 64
)

// LookupEnv is injectable so tests can load independent configurations without
// changing the process-wide environment.
type LookupEnv func(string) (string, bool)

// Config contains the validated runtime settings for the gateway. The API key
// is private and intentionally omitted from String, Format, and LogValue
// because it is a credential, not an operational diagnostic value.
type Config struct {
	apiKey                   string
	BaseURL                  string
	Model                    string
	Host                     string
	Port                     int
	AllowNonLoopback         bool
	LogLevel                 slog.Level
	ShutdownTimeout          time.Duration
	ReadHeaderTimeout        time.Duration
	IdleTimeout              time.Duration
	RequestBodyReadTimeout   time.Duration
	UpstreamConnectTimeout   time.Duration
	TLSHandshakeTimeout      time.Duration
	ResponseHeaderTimeout    time.Duration
	StreamIdleTimeout        time.Duration
	DownstreamWriteTimeout   time.Duration
	MaxBodyBytes             int64
	MaxHeaderBytes           int
	MaxInputItems            int
	MaxCollectionItems       int
	MaxTools                 int
	MaxSchemaBytes           int64
	MaxSSELineBytes          int
	MaxSSEEventBytes         int
	MaxSSEBufferedBytes      int
	MaxSSEReadBufferBytes    int
	MaxOutputBytes           int
	MaxTextBytes             int
	MaxReasoningBytes        int
	MaxToolCallArgumentBytes int
	MaxPendingTurnBytes      int64
	MaxPendingRecords        int
	MaxPendingAggregateBytes int64
	MaxActiveRequests        int
}

// Defaults returns the safe local defaults. Port zero remains a valid value
// when a caller intentionally requests an ephemeral test listener.
func Defaults() Config {
	return Config{
		BaseURL:                  DefaultBaseURL,
		Model:                    opencodego.DefaultModel,
		Host:                     DefaultHost,
		Port:                     DefaultPort,
		LogLevel:                 slog.LevelInfo,
		ShutdownTimeout:          DefaultShutdownTimeout,
		ReadHeaderTimeout:        DefaultReadHeaderTimeout,
		IdleTimeout:              DefaultIdleTimeout,
		RequestBodyReadTimeout:   DefaultRequestBodyReadTimeout,
		UpstreamConnectTimeout:   DefaultUpstreamConnectTimeout,
		TLSHandshakeTimeout:      DefaultTLSHandshakeTimeout,
		ResponseHeaderTimeout:    DefaultResponseHeaderTimeout,
		StreamIdleTimeout:        DefaultStreamIdleTimeout,
		DownstreamWriteTimeout:   DefaultDownstreamWriteTimeout,
		MaxBodyBytes:             DefaultMaxBodyBytes,
		MaxHeaderBytes:           DefaultMaxHeaderBytes,
		MaxInputItems:            DefaultMaxInputItems,
		MaxCollectionItems:       DefaultMaxCollectionItems,
		MaxTools:                 DefaultMaxTools,
		MaxSchemaBytes:           DefaultMaxSchemaBytes,
		MaxSSELineBytes:          DefaultMaxSSELineBytes,
		MaxSSEEventBytes:         DefaultMaxSSEEventBytes,
		MaxSSEBufferedBytes:      DefaultMaxSSEBufferedBytes,
		MaxSSEReadBufferBytes:    DefaultMaxSSEReadBufferBytes,
		MaxOutputBytes:           DefaultMaxOutputBytes,
		MaxTextBytes:             DefaultMaxTextBytes,
		MaxReasoningBytes:        DefaultMaxReasoningBytes,
		MaxToolCallArgumentBytes: DefaultMaxToolCallArgumentBytes,
		MaxPendingTurnBytes:      DefaultMaxPendingTurnBytes,
		MaxPendingRecords:        DefaultMaxPendingRecords,
		MaxPendingAggregateBytes: DefaultMaxPendingAggregateBytes,
		MaxActiveRequests:        DefaultMaxActiveRequests,
	}
}

// Load reads and validates the environment-backed runtime configuration.
func Load(lookup LookupEnv) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}

	config := Defaults()
	var ok bool
	if config.apiKey, ok = lookup("OPENCODE_GO_API_KEY"); !ok || strings.TrimSpace(config.apiKey) == "" {
		return Config{}, fmt.Errorf("OPENCODE_GO_API_KEY is required")
	}

	if value, present := lookup("OPENCODE_GO_BASE_URL"); present {
		config.BaseURL = value
	}
	if value, present := lookup("OPENCODE_GO_MODEL"); present {
		config.Model = value
	}
	if value, present := lookup("OPENCODE_GATEWAY_HOST"); present {
		config.Host = value
	}
	if value, present := lookup("OPENCODE_GATEWAY_PORT"); present {
		port, err := parsePort(value)
		if err != nil {
			return Config{}, err
		}
		config.Port = port
	}
	if value, present := lookup("OPENCODE_GATEWAY_LOG_LEVEL"); present {
		level, err := parseLogLevel(value)
		if err != nil {
			return Config{}, err
		}
		config.LogLevel = level
	}
	if value, present := lookup("OPENCODE_GATEWAY_ALLOW_NON_LOOPBACK"); present {
		allow, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("OPENCODE_GATEWAY_ALLOW_NON_LOOPBACK must be boolean")
		}
		config.AllowNonLoopback = allow
	}

	var err error
	if config.ShutdownTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_SHUTDOWN_TIMEOUT", config.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if config.ReadHeaderTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_READ_HEADER_TIMEOUT", config.ReadHeaderTimeout); err != nil {
		return Config{}, err
	}
	if config.IdleTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_IDLE_TIMEOUT", config.IdleTimeout); err != nil {
		return Config{}, err
	}
	if config.RequestBodyReadTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_REQUEST_BODY_READ_TIMEOUT", config.RequestBodyReadTimeout); err != nil {
		return Config{}, err
	}
	if config.UpstreamConnectTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_UPSTREAM_CONNECT_TIMEOUT", config.UpstreamConnectTimeout); err != nil {
		return Config{}, err
	}
	if config.TLSHandshakeTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_TLS_HANDSHAKE_TIMEOUT", config.TLSHandshakeTimeout); err != nil {
		return Config{}, err
	}
	if config.ResponseHeaderTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_RESPONSE_HEADER_TIMEOUT", config.ResponseHeaderTimeout); err != nil {
		return Config{}, err
	}
	if config.StreamIdleTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_STREAM_IDLE_TIMEOUT", config.StreamIdleTimeout); err != nil {
		return Config{}, err
	}
	if config.DownstreamWriteTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_DOWNSTREAM_WRITE_TIMEOUT", config.DownstreamWriteTimeout); err != nil {
		return Config{}, err
	}
	if config.MaxBodyBytes, err = optionalInt64(lookup, "OPENCODE_GATEWAY_MAX_BODY_BYTES", config.MaxBodyBytes); err != nil {
		return Config{}, err
	}
	if config.MaxHeaderBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_HEADER_BYTES", config.MaxHeaderBytes); err != nil {
		return Config{}, err
	}
	if config.MaxInputItems, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_INPUT_ITEMS", config.MaxInputItems); err != nil {
		return Config{}, err
	}
	if config.MaxCollectionItems, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_COLLECTION_ITEMS", config.MaxCollectionItems); err != nil {
		return Config{}, err
	}
	if config.MaxTools, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_TOOLS", config.MaxTools); err != nil {
		return Config{}, err
	}
	if config.MaxSchemaBytes, err = optionalInt64(lookup, "OPENCODE_GATEWAY_MAX_SCHEMA_BYTES", config.MaxSchemaBytes); err != nil {
		return Config{}, err
	}
	if config.MaxSSELineBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_SSE_LINE_BYTES", config.MaxSSELineBytes); err != nil {
		return Config{}, err
	}
	if config.MaxSSEEventBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_SSE_EVENT_BYTES", config.MaxSSEEventBytes); err != nil {
		return Config{}, err
	}
	if config.MaxSSEBufferedBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_SSE_BUFFERED_BYTES", config.MaxSSEBufferedBytes); err != nil {
		return Config{}, err
	}
	if config.MaxSSEReadBufferBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_SSE_READ_BUFFER_BYTES", config.MaxSSEReadBufferBytes); err != nil {
		return Config{}, err
	}
	if config.MaxOutputBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_OUTPUT_BYTES", config.MaxOutputBytes); err != nil {
		return Config{}, err
	}
	if config.MaxTextBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_TEXT_BYTES", config.MaxTextBytes); err != nil {
		return Config{}, err
	}
	if config.MaxReasoningBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_REASONING_BYTES", config.MaxReasoningBytes); err != nil {
		return Config{}, err
	}
	if config.MaxToolCallArgumentBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_TOOL_CALL_ARGUMENT_BYTES", config.MaxToolCallArgumentBytes); err != nil {
		return Config{}, err
	}
	if config.MaxPendingTurnBytes, err = optionalInt64(lookup, "OPENCODE_GATEWAY_MAX_PENDING_TURN_BYTES", config.MaxPendingTurnBytes); err != nil {
		return Config{}, err
	}
	if config.MaxPendingRecords, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_PENDING_RECORDS", config.MaxPendingRecords); err != nil {
		return Config{}, err
	}
	if config.MaxPendingAggregateBytes, err = optionalInt64(lookup, "OPENCODE_GATEWAY_MAX_PENDING_AGGREGATE_BYTES", config.MaxPendingAggregateBytes); err != nil {
		return Config{}, err
	}
	if config.MaxActiveRequests, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_ACTIVE_REQUESTS", config.MaxActiveRequests); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate checks configuration invariants before a listener or upstream
// client is started.
func (c Config) Validate() error {
	if strings.TrimSpace(c.apiKey) == "" {
		return fmt.Errorf("OPENCODE_GO_API_KEY is required")
	}
	if !validBaseURL(c.BaseURL) {
		return fmt.Errorf("OPENCODE_GO_BASE_URL must be an absolute HTTPS URL")
	}
	if err := opencodego.ValidateModel(c.Model); err != nil {
		return fmt.Errorf("OPENCODE_GO_MODEL is unsupported")
	}
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("OPENCODE_GATEWAY_HOST must not be empty")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("OPENCODE_GATEWAY_PORT must be between 0 and 65535")
	}
	if !c.AllowNonLoopback && !isLoopbackHost(c.Host) {
		return fmt.Errorf("OPENCODE_GATEWAY_HOST must resolve to a loopback address unless OPENCODE_GATEWAY_ALLOW_NON_LOOPBACK=true")
	}
	if !validLogLevel(c.LogLevel) {
		return fmt.Errorf("OPENCODE_GATEWAY_LOG_LEVEL is unsupported")
	}
	if err := validatePositiveDuration("OPENCODE_GATEWAY_SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return err
	}
	if err := validatePositiveDuration("OPENCODE_GATEWAY_READ_HEADER_TIMEOUT", c.ReadHeaderTimeout); err != nil {
		return err
	}
	if err := validatePositiveDuration("OPENCODE_GATEWAY_IDLE_TIMEOUT", c.IdleTimeout); err != nil {
		return err
	}
	for _, phase := range []struct {
		key   string
		value time.Duration
	}{
		{key: "OPENCODE_GATEWAY_REQUEST_BODY_READ_TIMEOUT", value: c.RequestBodyReadTimeout},
		{key: "OPENCODE_GATEWAY_UPSTREAM_CONNECT_TIMEOUT", value: c.UpstreamConnectTimeout},
		{key: "OPENCODE_GATEWAY_TLS_HANDSHAKE_TIMEOUT", value: c.TLSHandshakeTimeout},
		{key: "OPENCODE_GATEWAY_RESPONSE_HEADER_TIMEOUT", value: c.ResponseHeaderTimeout},
		{key: "OPENCODE_GATEWAY_STREAM_IDLE_TIMEOUT", value: c.StreamIdleTimeout},
		{key: "OPENCODE_GATEWAY_DOWNSTREAM_WRITE_TIMEOUT", value: c.DownstreamWriteTimeout},
	} {
		if err := validatePositiveDuration(phase.key, phase.value); err != nil {
			return err
		}
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_BODY_BYTES must be positive")
	}
	if c.MaxHeaderBytes <= 0 {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_HEADER_BYTES must be positive")
	}
	for _, limit := range []struct {
		key   string
		value int
	}{
		{key: "OPENCODE_GATEWAY_MAX_INPUT_ITEMS", value: c.MaxInputItems},
		{key: "OPENCODE_GATEWAY_MAX_COLLECTION_ITEMS", value: c.MaxCollectionItems},
		{key: "OPENCODE_GATEWAY_MAX_TOOLS", value: c.MaxTools},
		{key: "OPENCODE_GATEWAY_MAX_SSE_LINE_BYTES", value: c.MaxSSELineBytes},
		{key: "OPENCODE_GATEWAY_MAX_SSE_EVENT_BYTES", value: c.MaxSSEEventBytes},
		{key: "OPENCODE_GATEWAY_MAX_SSE_BUFFERED_BYTES", value: c.MaxSSEBufferedBytes},
		{key: "OPENCODE_GATEWAY_MAX_SSE_READ_BUFFER_BYTES", value: c.MaxSSEReadBufferBytes},
		{key: "OPENCODE_GATEWAY_MAX_OUTPUT_BYTES", value: c.MaxOutputBytes},
		{key: "OPENCODE_GATEWAY_MAX_TEXT_BYTES", value: c.MaxTextBytes},
		{key: "OPENCODE_GATEWAY_MAX_REASONING_BYTES", value: c.MaxReasoningBytes},
		{key: "OPENCODE_GATEWAY_MAX_TOOL_CALL_ARGUMENT_BYTES", value: c.MaxToolCallArgumentBytes},
		{key: "OPENCODE_GATEWAY_MAX_ACTIVE_REQUESTS", value: c.MaxActiveRequests},
		{key: "OPENCODE_GATEWAY_MAX_PENDING_RECORDS", value: c.MaxPendingRecords},
	} {
		if limit.value <= 0 {
			return fmt.Errorf("%s must be positive", limit.key)
		}
	}
	if c.MaxTools > bridge.DefaultMaxProviderTools {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_TOOLS must not exceed %d", bridge.DefaultMaxProviderTools)
	}
	if c.MaxSchemaBytes <= 0 {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_SCHEMA_BYTES must be positive")
	}
	if c.MaxSchemaBytes > int64(bridge.DefaultMaxFunctionSchemaBytes) {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_SCHEMA_BYTES must not exceed %d", bridge.DefaultMaxFunctionSchemaBytes)
	}
	if c.MaxPendingTurnBytes <= 0 {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_PENDING_TURN_BYTES must be positive")
	}
	if c.MaxPendingAggregateBytes <= 0 {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_PENDING_AGGREGATE_BYTES must be positive")
	}
	return nil
}

func (c Config) ListenAddr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// APIKey returns the credential for the future upstream client. Callers must
// never include it in logs, errors, or request data sent to Codex.
func (c Config) APIKey() string {
	return c.apiKey
}

// WithAPIKey is intended for composition in tests and application wiring; it
// does not validate the value until Validate is called.
func (c Config) WithAPIKey(value string) Config {
	c.apiKey = value
	return c
}

func (c Config) String() string {
	return fmt.Sprintf(
		"api_key=<redacted> base_url=%q model=%q host=%q port=%d allow_non_loopback=%t log_level=%s shutdown_timeout=%s read_header_timeout=%s request_body_read_timeout=%s upstream_connect_timeout=%s tls_handshake_timeout=%s response_header_timeout=%s stream_idle_timeout=%s downstream_write_timeout=%s idle_timeout=%s max_body_bytes=%d max_header_bytes=%d max_input_items=%d max_collection_items=%d max_tools=%d max_schema_bytes=%d max_sse_line_bytes=%d max_sse_event_bytes=%d max_sse_buffered_bytes=%d max_sse_read_buffer_bytes=%d max_output_bytes=%d max_text_bytes=%d max_reasoning_bytes=%d max_tool_call_argument_bytes=%d max_pending_turn_bytes=%d max_pending_records=%d max_pending_aggregate_bytes=%d max_active_requests=%d",
		c.BaseURL,
		c.Model,
		c.Host,
		c.Port,
		c.AllowNonLoopback,
		c.LogLevel,
		c.ShutdownTimeout,
		c.ReadHeaderTimeout,
		c.RequestBodyReadTimeout,
		c.UpstreamConnectTimeout,
		c.TLSHandshakeTimeout,
		c.ResponseHeaderTimeout,
		c.StreamIdleTimeout,
		c.DownstreamWriteTimeout,
		c.IdleTimeout,
		c.MaxBodyBytes,
		c.MaxHeaderBytes,
		c.MaxInputItems,
		c.MaxCollectionItems,
		c.MaxTools,
		c.MaxSchemaBytes,
		c.MaxSSELineBytes,
		c.MaxSSEEventBytes,
		c.MaxSSEBufferedBytes,
		c.MaxSSEReadBufferBytes,
		c.MaxOutputBytes,
		c.MaxTextBytes,
		c.MaxReasoningBytes,
		c.MaxToolCallArgumentBytes,
		c.MaxPendingTurnBytes,
		c.MaxPendingRecords,
		c.MaxPendingAggregateBytes,
		c.MaxActiveRequests,
	)
}

func (c Config) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(c.String()))
}

func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", c.BaseURL),
		slog.String("model", c.Model),
		slog.String("host", c.Host),
		slog.Int("port", c.Port),
		slog.Bool("allow_non_loopback", c.AllowNonLoopback),
		slog.Any("log_level", c.LogLevel),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Duration("read_header_timeout", c.ReadHeaderTimeout),
		slog.Duration("request_body_read_timeout", c.RequestBodyReadTimeout),
		slog.Duration("upstream_connect_timeout", c.UpstreamConnectTimeout),
		slog.Duration("tls_handshake_timeout", c.TLSHandshakeTimeout),
		slog.Duration("response_header_timeout", c.ResponseHeaderTimeout),
		slog.Duration("stream_idle_timeout", c.StreamIdleTimeout),
		slog.Duration("downstream_write_timeout", c.DownstreamWriteTimeout),
		slog.Duration("idle_timeout", c.IdleTimeout),
		slog.Int64("max_body_bytes", c.MaxBodyBytes),
		slog.Int("max_header_bytes", c.MaxHeaderBytes),
		slog.Int("max_input_items", c.MaxInputItems),
		slog.Int("max_collection_items", c.MaxCollectionItems),
		slog.Int("max_tools", c.MaxTools),
		slog.Int64("max_schema_bytes", c.MaxSchemaBytes),
		slog.Int("max_sse_line_bytes", c.MaxSSELineBytes),
		slog.Int("max_sse_event_bytes", c.MaxSSEEventBytes),
		slog.Int("max_sse_buffered_bytes", c.MaxSSEBufferedBytes),
		slog.Int("max_sse_read_buffer_bytes", c.MaxSSEReadBufferBytes),
		slog.Int("max_output_bytes", c.MaxOutputBytes),
		slog.Int("max_text_bytes", c.MaxTextBytes),
		slog.Int("max_reasoning_bytes", c.MaxReasoningBytes),
		slog.Int("max_tool_call_argument_bytes", c.MaxToolCallArgumentBytes),
		slog.Int64("max_pending_turn_bytes", c.MaxPendingTurnBytes),
		slog.Int("max_pending_records", c.MaxPendingRecords),
		slog.Int64("max_pending_aggregate_bytes", c.MaxPendingAggregateBytes),
		slog.Int("max_active_requests", c.MaxActiveRequests),
	)
}

func validBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	addresses, err := net.LookupIP(host)
	if err != nil || len(addresses) == 0 {
		return false
	}
	for _, address := range addresses {
		if !address.IsLoopback() {
			return false
		}
	}
	return true
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 0 || port > 65535 {
		return 0, fmt.Errorf("OPENCODE_GATEWAY_PORT must be between 0 and 65535")
	}
	return port, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("OPENCODE_GATEWAY_LOG_LEVEL must be debug, info, warn, or error")
	}
}

func validLogLevel(level slog.Level) bool {
	return level == slog.LevelDebug || level == slog.LevelInfo || level == slog.LevelWarn || level == slog.LevelError
}

func optionalDuration(lookup LookupEnv, key string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return duration, nil
}

func optionalInt64(lookup LookupEnv, key string, fallback int64) (int64, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func optionalInt(lookup LookupEnv, key string, fallback int) (int, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func validatePositiveDuration(key string, value time.Duration) error {
	if value <= 0 {
		return fmt.Errorf("%s must be a positive duration", key)
	}
	return nil
}
