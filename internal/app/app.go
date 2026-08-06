package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"unicode"

	"github.com/rafaself/opencode-go-gateway/internal/config"
	"github.com/rafaself/opencode-go-gateway/internal/server"
)

// Run starts the local gateway and owns its bounded graceful-shutdown
// lifecycle. Signal wiring belongs to the CLI so this function stays easy to
// exercise with a cancellable context in integration tests.
func Run(ctx context.Context, settings config.Config, logger *slog.Logger, onReady func(string)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = NewLogger(io.Discard, settings.LogLevel)
	}

	runtimeServer, err := server.New(server.Config{
		ListenAddr:        settings.ListenAddr(),
		AllowNonLoopback:  settings.AllowNonLoopback,
		ReadHeaderTimeout: settings.ReadHeaderTimeout,
		ReadTimeout:       settings.ReadTimeout,
		WriteTimeout:      settings.WriteTimeout,
		IdleTimeout:       settings.IdleTimeout,
		MaxBodyBytes:      settings.MaxBodyBytes,
		MaxHeaderBytes:    settings.MaxHeaderBytes,
	}, logger)
	if err != nil {
		return err
	}
	defer func() { _ = runtimeServer.Close() }()

	serveErr := make(chan error, 1)
	go func() { serveErr <- runtimeServer.Serve() }()

	logger.Info("server listening",
		slog.String("component", "app"),
		slog.String("address", runtimeServer.Addr()),
	)
	if onReady != nil {
		onReady(runtimeServer.Addr())
	}

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("server shutdown",
			slog.String("component", "app"),
			slog.String("reason", "signal"),
		)
		shutdownContext, cancel := context.WithTimeout(context.Background(), settings.ShutdownTimeout)
		defer cancel()
		shutdownErr := runtimeServer.Shutdown(shutdownContext)
		serveErr := <-serveErr
		if shutdownErr != nil {
			return fmt.Errorf("server shutdown failed: %w", shutdownErr)
		}
		if serveErr != nil {
			return serveErr
		}
		return nil
	}
}

// NewLogger creates the process logger. Attribute-level redaction is a
// defense-in-depth measure for future callers; request handlers still avoid
// constructing sensitive attributes entirely.
func NewLogger(output io.Writer, level slog.Level) *slog.Logger {
	if output == nil {
		output = io.Discard
	}
	if level != slog.LevelDebug && level != slog.LevelInfo && level != slog.LevelWarn && level != slog.LevelError {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redactLogAttribute,
	}))
}

func redactLogAttribute(groups []string, attribute slog.Attr) slog.Attr {
	if isSensitiveLogKey(attribute.Key) || sensitiveLogGroup(groups) {
		return slog.String(attribute.Key, "<redacted>")
	}
	if attribute.Value.Kind() == slog.KindAny {
		return slog.Any(attribute.Key, redactLogAny(attribute.Value.Any(), false, 0))
	}
	return attribute
}

func sensitiveLogGroup(groups []string) bool {
	for _, group := range groups {
		if isSensitiveLogKey(group) {
			return true
		}
	}
	return false
}

var sensitiveLogKeyMarkers = []string{
	"authorization", "api_key", "apikey", "access_token", "auth_token", "credential", "password", "passphrase", "secret", "private_key", "request_body", "response_body", "prompt", "instruction", "input", "content", "tool_arguments", "tool_output", "environment", "source_code", "filesystem_path", "file_path", "working_directory", "workdir", "cwd", "client_metadata", "cookie", "path",
}

func isSensitiveLogKey(key string) bool {
	normalized := normalizeLogKey(key)
	for _, marker := range sensitiveLogKeyMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return normalized == "token" || strings.HasPrefix(normalized, "token_") || strings.HasSuffix(normalized, "_token")
}

func normalizeLogKey(key string) string {
	var normalized strings.Builder
	runes := []rune(key)
	for index, current := range runes {
		if unicode.IsUpper(current) {
			previousIsLowerOrDigit := index > 0 && (unicode.IsLower(runes[index-1]) || unicode.IsDigit(runes[index-1]))
			nextIsLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if previousIsLowerOrDigit || nextIsLower {
				normalized.WriteByte('_')
			}
			current = unicode.ToLower(current)
		}
		if unicode.IsLetter(current) || unicode.IsDigit(current) || current == '_' {
			normalized.WriteRune(current)
			continue
		}
		normalized.WriteByte('_')
	}
	return normalized.String()
}

func redactLogAny(value any, sensitive bool, depth int) any {
	if sensitive {
		return "<redacted>"
	}
	if depth > 32 {
		return "<redacted>"
	}
	return redactLogReflectValue(reflect.ValueOf(value), depth)
}

func redactLogReflectValue(value reflect.Value, depth int) any {
	if !value.IsValid() {
		return nil
	}
	if depth > 32 {
		return "<redacted>"
	}

	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		return redactLogReflectValue(value.Elem(), depth+1)
	case reflect.Map:
		if value.IsNil() || value.Type().Key().Kind() != reflect.String {
			return "<redacted>"
		}
		result := make(map[string]any, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			childSensitive := isSensitiveLogKey(key)
			outputKey := key
			if childSensitive {
				outputKey = "<redacted>"
			}
			result[outputKey] = redactLogAny(iter.Value().Interface(), childSensitive, depth+1)
		}
		return result
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return "<redacted>"
		}
		result := make([]any, value.Len())
		for index := 0; index < value.Len(); index++ {
			result[index] = redactLogReflectValue(value.Index(index), depth+1)
		}
		return result
	case reflect.String:
		return value.String()
	case reflect.Bool:
		return value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint()
	case reflect.Float32, reflect.Float64:
		return value.Float()
	default:
		return "<redacted>"
	}
}
