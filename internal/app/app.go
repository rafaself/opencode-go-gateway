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
	"github.com/rafaself/opencode-go-gateway/internal/opencodego"
	"github.com/rafaself/opencode-go-gateway/internal/server"
)

// BuildMetadata is supplied by the CLI's linker-injected build variables and
// is used only for safe diagnostics and the upstream User-Agent.
type BuildMetadata struct {
	Version   string
	Commit    string
	BuildDate string
}

// Run starts the local gateway and owns its bounded graceful-shutdown
// lifecycle. Signal wiring belongs to the CLI so this function stays easy to
// exercise with a cancellable context in integration tests.
func Run(ctx context.Context, settings config.Config, logger *slog.Logger, onReady func(string)) error {
	return RunWithBuildMetadata(ctx, settings, logger, onReady, BuildMetadata{
		Version:   "dev",
		Commit:    "unknown",
		BuildDate: "unknown",
	})
}

// RunWithBuildMetadata constructs the configured OpenCode Go client and wires
// it into the gateway. The API key crosses this boundary only into the
// provider client; it is never passed to the Codex-facing server.
func RunWithBuildMetadata(ctx context.Context, settings config.Config, logger *slog.Logger, onReady func(string), metadata BuildMetadata) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = NewLogger(io.Discard, settings.LogLevel)
	}
	upstreamClient, err := opencodego.NewClient(opencodego.ClientConfig{
		APIKey:                settings.APIKey(),
		BaseURL:               settings.BaseURL,
		Model:                 settings.Model,
		UserAgent:             buildUserAgent(metadata),
		DialTimeout:           settings.UpstreamConnectTimeout,
		TLSHandshakeTimeout:   settings.TLSHandshakeTimeout,
		ResponseHeaderTimeout: settings.ResponseHeaderTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure upstream client: %w", err)
	}

	runtimeServer, err := server.New(server.Config{
		ListenAddr:               settings.ListenAddr(),
		AllowNonLoopback:         settings.AllowNonLoopback,
		Model:                    settings.Model,
		Upstream:                 server.NewOpenCodeUpstreamClient(upstreamClient),
		ReadHeaderTimeout:        settings.ReadHeaderTimeout,
		IdleTimeout:              settings.IdleTimeout,
		RequestBodyReadTimeout:   settings.RequestBodyReadTimeout,
		StreamIdleTimeout:        settings.StreamIdleTimeout,
		DownstreamWriteTimeout:   settings.DownstreamWriteTimeout,
		MaxBodyBytes:             settings.MaxBodyBytes,
		MaxHeaderBytes:           settings.MaxHeaderBytes,
		MaxInputItems:            settings.MaxInputItems,
		MaxCollectionItems:       settings.MaxCollectionItems,
		MaxTools:                 settings.MaxTools,
		MaxSchemaBytes:           settings.MaxSchemaBytes,
		MaxSSELineBytes:          settings.MaxSSELineBytes,
		MaxSSEEventBytes:         settings.MaxSSEEventBytes,
		MaxSSEBufferedBytes:      settings.MaxSSEBufferedBytes,
		MaxSSEReadBufferBytes:    settings.MaxSSEReadBufferBytes,
		MaxOutputBytes:           settings.MaxOutputBytes,
		MaxTextBytes:             settings.MaxTextBytes,
		MaxReasoningBytes:        settings.MaxReasoningBytes,
		MaxToolCallArgumentBytes: settings.MaxToolCallArgumentBytes,
		MaxPendingTurnBytes:      settings.MaxPendingTurnBytes,
		MaxPendingRecords:        settings.MaxPendingRecords,
		MaxPendingAggregateBytes: settings.MaxPendingAggregateBytes,
		MaxActiveRequests:        settings.MaxActiveRequests,
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

func buildUserAgent(metadata BuildMetadata) string {
	return "opencode-go-gateway/" + safeMetadataPart(metadata.Version) +
		" commit/" + safeMetadataPart(metadata.Commit) +
		" build/" + safeMetadataPart(metadata.BuildDate)
}

func safeMetadataPart(value string) string {
	if value == "" {
		return "unknown"
	}
	var result strings.Builder
	for _, runeValue := range value {
		if (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= 'A' && runeValue <= 'Z') || (runeValue >= '0' && runeValue <= '9') || runeValue == '.' || runeValue == '-' || runeValue == '_' {
			result.WriteRune(runeValue)
			continue
		}
		result.WriteByte('-')
	}
	if result.Len() == 0 {
		return "unknown"
	}
	if result.Len() > 64 {
		return result.String()[:64]
	}
	return result.String()
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
