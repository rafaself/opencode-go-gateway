package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxBodyBytes  int64 = 16 << 20
	fixtureSchemaVersion       = 1
)

type Config struct {
	ListenAddr    string
	OutputDir     string
	FixturePrefix string
	CodexVersion  string
	ResponseMode  ResponseMode
	ResponseText  string
	MaxBodyBytes  int64
	OneShot       bool
	OnCapture     func(CaptureInfo)
}

type CaptureInfo struct {
	Path         string
	Sequence     int
	CodexVersion string
}

type Server struct {
	config Config
	ln     net.Listener
	http   *http.Server

	mu       sync.Mutex
	sequence int
}

type Fixture struct {
	SchemaVersion int             `json:"schema_version"`
	FixtureName   string          `json:"fixture_name"`
	CaptureNumber int             `json:"capture_number"`
	CodexVersion  string          `json:"codex_version"`
	Request       RequestFixture  `json:"request"`
	Response      ResponseFixture `json:"response"`
}

type RequestFixture struct {
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Headers        map[string]string `json:"headers"`
	Body           any               `json:"body"`
	TopLevelFields []string          `json:"top_level_fields"`
	InputItemTypes []string          `json:"input_item_types"`
	ToolTypes      []string          `json:"tool_types"`
}

type ResponseFixture struct {
	Mode       ResponseMode `json:"mode"`
	EventTypes []string     `json:"event_types"`
	DoneMarker bool         `json:"done_marker"`
}

func Listen(config Config) (*Server, error) {
	config = withDefaults(config)
	listenAddress, err := resolveLoopbackAddr(config.ListenAddr)
	if err != nil {
		return nil, err
	}
	if _, err := ParseResponseMode(string(config.ResponseMode)); err != nil {
		return nil, err
	}
	if config.CodexVersion != "" && !codexVersionValuePattern.MatchString(config.CodexVersion) {
		return nil, fmt.Errorf("invalid Codex version %q", config.CodexVersion)
	}
	if config.MaxBodyBytes <= 0 {
		return nil, fmt.Errorf("max body bytes must be positive")
	}
	if err := validateFixturePrefix(config.FixturePrefix); err != nil {
		return nil, err
	}

	ln, err := net.ListenTCP("tcp", listenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", config.ListenAddr, err)
	}

	server := &Server{config: config, ln: ln}
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	return server, nil
}

func withDefaults(config Config) Config {
	if config.ListenAddr == "" {
		config.ListenAddr = "127.0.0.1:0"
	}
	if config.FixturePrefix == "" {
		config.FixturePrefix = "capture"
	}
	if config.ResponseMode == "" {
		config.ResponseMode = ResponseText
	}
	if config.ResponseText == "" {
		config.ResponseText = "capture acknowledged"
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return config
}

func (s *Server) BaseURL() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return "http://" + s.ln.Addr().String() + "/v1"
}

func (s *Server) Addr() net.Addr {
	if s == nil || s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	err := s.http.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Close() error {
	if s == nil || s.http == nil {
		return nil
	}
	return s.http.Close()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/responses" {
		writeJSONError(w, http.StatusNotFound, "only POST /v1/responses is supported")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "only POST /v1/responses is supported")
		return
	}

	body, err := decodeRequestBody(w, r, s.config.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be a single JSON object")
		return
	}

	sequence := s.nextSequence()
	version := s.config.CodexVersion
	if version == "" {
		version = codexVersionFromUserAgent(r.Header.Get("User-Agent"))
	}

	mode := s.config.ResponseMode
	if containsToolResult(body) && (mode == ResponseFunction || mode == ResponseParallel || mode == ResponseCustom) {
		mode = ResponseText
	}
	events, err := responseEvents(mode, responseModel(body), s.config.ResponseText, sequence)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not construct response fixture")
		return
	}

	fixture := Fixture{
		SchemaVersion: fixtureSchemaVersion,
		FixtureName:   fmt.Sprintf("%s-%03d", s.config.FixturePrefix, sequence),
		CaptureNumber: sequence,
		CodexVersion:  version,
		Request: RequestFixture{
			Method:         r.Method,
			Path:           r.URL.Path,
			Headers:        sanitizeHeaders(r.Header, version),
			Body:           Redact(body),
			TopLevelFields: topLevelFields(body),
			InputItemTypes: itemTypes(body, "input"),
			ToolTypes:      itemTypes(body, "tools"),
		},
		Response: ResponseFixture{
			Mode:       mode,
			EventTypes: eventTypes(events),
			DoneMarker: false,
		},
	}

	path, err := s.writeFixture(fixture)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not write redacted fixture")
		return
	}
	if s.config.OnCapture != nil {
		s.config.OnCapture(CaptureInfo{Path: path, Sequence: sequence, CodexVersion: version})
	}

	if err := writeResponseStream(w, events); err != nil {
		return
	}
	if s.config.OneShot {
		go s.Close()
	}
}

func decodeRequestBody(w http.ResponseWriter, r *http.Request, maxBytes int64) (any, error) {
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	defer limited.Close()
	decoder := json.NewDecoder(limited)
	var body any
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("body is null")
	}
	if object, ok := body.(map[string]any); !ok || object == nil {
		return nil, fmt.Errorf("body is not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("body has trailing JSON")
		}
		return nil, err
	}
	return body, nil
}

func (s *Server) nextSequence() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	return s.sequence
}

func (s *Server) writeFixture(fixture Fixture) (string, error) {
	if s.config.OutputDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(s.config.OutputDir, 0o750); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("%s-%03d.json", s.config.FixturePrefix, fixture.CaptureNumber)
	path := filepath.Join(s.config.OutputDir, filename)
	temp, err := os.CreateTemp(s.config.OutputDir, s.config.FixturePrefix+"-*.tmp")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return "", err
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(fixture); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tempName, path); err != nil {
		return "", err
	}
	if err := os.Remove(tempName); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func resolveLoopbackAddr(address string) (*net.TCPAddr, error) {
	resolved, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	if resolved.IP == nil || !resolved.IP.IsLoopback() {
		return nil, fmt.Errorf("listen address %q must resolve to a loopback host", address)
	}
	return resolved, nil
}

func validateFixturePrefix(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("fixture prefix must not be empty")
	}
	if filepath.Base(prefix) != prefix || strings.ContainsAny(prefix, `/\\`) {
		return fmt.Errorf("fixture prefix must be a filename component")
	}
	for _, r := range prefix {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("fixture prefix contains unsupported character %q", r)
	}
	return nil
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "invalid_request_error",
			"message": message,
		},
	})
}

func topLevelFields(body any) []string {
	object, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func itemTypes(body any, field string) []string {
	object, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	items, ok := object[field].([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	for _, item := range items {
		itemObject, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, ok := itemObject["type"].(string)
		if ok && itemType != "" {
			seen[itemType] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for itemType := range seen {
		result = append(result, itemType)
	}
	sort.Strings(result)
	return result
}

func responseModel(body any) string {
	object, ok := body.(map[string]any)
	if !ok {
		return "capture-model"
	}
	model, ok := object["model"].(string)
	if !ok || model == "" {
		return "capture-model"
	}
	return model
}

func containsToolResult(value any) bool {
	switch item := value.(type) {
	case map[string]any:
		if itemType, ok := item["type"].(string); ok && strings.HasSuffix(itemType, "_output") {
			return true
		}
		for _, child := range item {
			if containsToolResult(child) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if containsToolResult(child) {
				return true
			}
		}
	}
	return false
}
