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
)

const (
	DefaultBaseURL = "https://opencode.ai/zen/go/v1"
	DefaultHost    = "127.0.0.1"
	DefaultPort    = 8787

	DefaultShutdownTimeout   = 10 * time.Second
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
	DefaultMaxBodyBytes      = int64(16 << 20)
	DefaultMaxHeaderBytes    = 64 << 10
)

// LookupEnv is injectable so tests can load independent configurations without
// changing the process-wide environment.
type LookupEnv func(string) (string, bool)

// Config contains the validated runtime settings for the gateway. The API key
// is private and intentionally omitted from String, Format, and LogValue
// because it is a credential, not an operational diagnostic value.
type Config struct {
	apiKey            string
	BaseURL           string
	Host              string
	Port              int
	AllowNonLoopback  bool
	LogLevel          slog.Level
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxBodyBytes      int64
	MaxHeaderBytes    int
}

// Defaults returns the safe local defaults. Port zero remains a valid value
// when a caller intentionally requests an ephemeral test listener.
func Defaults() Config {
	return Config{
		BaseURL:           DefaultBaseURL,
		Host:              DefaultHost,
		Port:              DefaultPort,
		LogLevel:          slog.LevelInfo,
		ShutdownTimeout:   DefaultShutdownTimeout,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		ReadTimeout:       DefaultReadTimeout,
		WriteTimeout:      DefaultWriteTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		MaxBodyBytes:      DefaultMaxBodyBytes,
		MaxHeaderBytes:    DefaultMaxHeaderBytes,
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
	if config.ReadTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_READ_TIMEOUT", config.ReadTimeout); err != nil {
		return Config{}, err
	}
	if config.WriteTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_WRITE_TIMEOUT", config.WriteTimeout); err != nil {
		return Config{}, err
	}
	if config.IdleTimeout, err = optionalDuration(lookup, "OPENCODE_GATEWAY_IDLE_TIMEOUT", config.IdleTimeout); err != nil {
		return Config{}, err
	}
	if config.MaxBodyBytes, err = optionalInt64(lookup, "OPENCODE_GATEWAY_MAX_BODY_BYTES", config.MaxBodyBytes); err != nil {
		return Config{}, err
	}
	if config.MaxHeaderBytes, err = optionalInt(lookup, "OPENCODE_GATEWAY_MAX_HEADER_BYTES", config.MaxHeaderBytes); err != nil {
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
	if err := validatePositiveDuration("OPENCODE_GATEWAY_READ_TIMEOUT", c.ReadTimeout); err != nil {
		return err
	}
	if err := validatePositiveDuration("OPENCODE_GATEWAY_WRITE_TIMEOUT", c.WriteTimeout); err != nil {
		return err
	}
	if err := validatePositiveDuration("OPENCODE_GATEWAY_IDLE_TIMEOUT", c.IdleTimeout); err != nil {
		return err
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_BODY_BYTES must be positive")
	}
	if c.MaxHeaderBytes <= 0 {
		return fmt.Errorf("OPENCODE_GATEWAY_MAX_HEADER_BYTES must be positive")
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
		"api_key=<redacted> base_url=%q host=%q port=%d allow_non_loopback=%t log_level=%s shutdown_timeout=%s read_header_timeout=%s read_timeout=%s write_timeout=%s idle_timeout=%s max_body_bytes=%d max_header_bytes=%d",
		c.BaseURL,
		c.Host,
		c.Port,
		c.AllowNonLoopback,
		c.LogLevel,
		c.ShutdownTimeout,
		c.ReadHeaderTimeout,
		c.ReadTimeout,
		c.WriteTimeout,
		c.IdleTimeout,
		c.MaxBodyBytes,
		c.MaxHeaderBytes,
	)
}

func (c Config) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(c.String()))
}

func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", c.BaseURL),
		slog.String("host", c.Host),
		slog.Int("port", c.Port),
		slog.Bool("allow_non_loopback", c.AllowNonLoopback),
		slog.Any("log_level", c.LogLevel),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Duration("read_header_timeout", c.ReadHeaderTimeout),
		slog.Duration("read_timeout", c.ReadTimeout),
		slog.Duration("write_timeout", c.WriteTimeout),
		slog.Duration("idle_timeout", c.IdleTimeout),
		slog.Int64("max_body_bytes", c.MaxBodyBytes),
		slog.Int("max_header_bytes", c.MaxHeaderBytes),
	)
}

func validBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return false
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
