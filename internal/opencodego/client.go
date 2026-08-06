package opencodego

import (
	"bytes"
	"context"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"encoding/json"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

const (
	defaultDialTimeout           = 10 * time.Second
	defaultKeepAlive             = 30 * time.Second
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultExpectContinueTimeout = 1 * time.Second
	defaultMaxIdleConnections    = 100
	defaultMaxIdlePerHost        = 10
	defaultIdleConnectionTimeout = 90 * time.Second
)

// HTTPDoer is the narrow dependency required by Client. *http.Client is the
// normal implementation, while httptest clients and deterministic fakes can
// be injected without changing process-global HTTP state.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ClientConfig configures one independent OpenCode Go client.
type ClientConfig struct {
	APIKey              string
	BaseURL             string
	Model               string
	UserAgent           string
	HTTPClient          HTTPDoer
	ThinkingMode        ThinkingMode
	MaxRequestBodyBytes int64
	MaxErrorBodyBytes   int64
}

// Client owns no mutable global state. Its HTTP request uses request context
// cancellation and phase-specific transport timeouts; there is intentionally
// no total http.Client.Timeout so a legitimate SSE stream can remain open.
type Client struct {
	httpClient          HTTPDoer
	endpoint            *url.URL
	apiKey              string
	model               string
	userAgent           string
	thinkingMode        ThinkingMode
	maxRequestBodyBytes int64
	maxErrorBodyBytes   int64
}

// NewClient constructs an independent provider client. An empty BaseURL,
// Model, UserAgent, or limit selects the documented safe default.
func NewClient(config ClientConfig) (*Client, error) {
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	endpoint, err := endpointURL(baseURL)
	if err != nil {
		return nil, err
	}
	if !validCredential(config.APIKey) {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}
	model := config.Model
	if model == "" {
		model = DefaultModel
	}
	if err := validateProviderModel(model); err != nil {
		return nil, err
	}
	userAgent := config.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	if !validHeaderValue(userAgent, 256) {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}
	maxRequestBodyBytes := config.MaxRequestBodyBytes
	if maxRequestBodyBytes == 0 {
		maxRequestBodyBytes = DefaultMaxRequestBodyBytes
	}
	if maxRequestBodyBytes <= 0 {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}
	maxErrorBodyBytes := config.MaxErrorBodyBytes
	if maxErrorBodyBytes == 0 {
		maxErrorBodyBytes = DefaultMaxErrorBodyBytes
	}
	if maxErrorBodyBytes <= 0 {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = newDefaultHTTPClient()
	}
	mode := config.ThinkingMode
	if mode != ThinkingDefault && mode != ThinkingEnabled && mode != ThinkingDisabled {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}
	if mode == ThinkingDefault {
		mode = ThinkingEnabled
	}
	return &Client{
		httpClient:          httpClient,
		endpoint:            endpoint,
		apiKey:              config.APIKey,
		model:               model,
		userAgent:           userAgent,
		thinkingMode:        mode,
		maxRequestBodyBytes: maxRequestBodyBytes,
		maxErrorBodyBytes:   maxErrorBodyBytes,
	}, nil
}

// New is a small composition helper for the documented defaults. It does not
// create or mutate a package-global client.
func New(apiKey string) (*Client, error) {
	return NewClient(ClientConfig{APIKey: apiKey})
}

// BuildRequest exposes the pure bridge-to-provider mapping selected by this
// client, which is useful to #7 before it starts a network operation.
func (client *Client) BuildRequest(request bridge.Request) (ChatCompletionRequest, error) {
	if client == nil {
		return ChatCompletionRequest{}, providerError(ErrorInvalidConfiguration, nil)
	}
	return MapRequestWithThinking(request, client.model, client.thinkingMode)
}

// Do sends one streaming Chat Completions request. On success the returned
// response body is still open and owned by the caller. Every error path closes
// a response body before returning.
func (client *Client) Do(ctx context.Context, request bridge.Request) (*Response, error) {
	if client == nil || client.httpClient == nil || client.endpoint == nil {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}
	if ctx == nil {
		return nil, providerError(ErrorInvalidRequest, nil)
	}
	wire, err := client.BuildRequest(request)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, providerError(ErrorInvalidRequest, err)
	}
	if int64(len(body)) > client.maxRequestBodyBytes {
		return nil, providerError(ErrorRequestTooLarge, nil)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, providerError(ErrorInvalidConfiguration, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+client.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", DefaultAccept)
	httpRequest.Header.Set("User-Agent", client.userAgent)

	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &ProviderError{Code: ErrorCanceled, cause: ctxErr}
		}
		return nil, &ProviderError{Code: ErrorNetwork, cause: err}
	}
	if response == nil {
		return nil, providerError(ErrorNetwork, nil)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, client.classifyUpstreamError(response)
	}
	if response.Body == nil {
		return nil, client.closeWithError(response, providerError(ErrorMalformedResponse, nil))
	}
	contentType := response.Header.Get("Content-Type")
	mediaType, _, parseErr := mime.ParseMediaType(contentType)
	if parseErr != nil || !strings.EqualFold(mediaType, DefaultAccept) {
		err := providerError(ErrorUnsupportedContentType, nil)
		err.ContentType = safeMediaType(contentType)
		return nil, client.closeWithError(response, err)
	}
	return &Response{
		StatusCode:  response.StatusCode,
		Header:      response.Header.Clone(),
		ContentType: contentType,
		Body:        response.Body,
	}, nil
}

func (client *Client) classifyUpstreamError(response *http.Response) error {
	code := ErrorUnexpectedStatus
	switch {
	case response.StatusCode == http.StatusBadRequest:
		code = ErrorBadRequest
	case response.StatusCode == http.StatusUnauthorized:
		code = ErrorUnauthorized
	case response.StatusCode == http.StatusForbidden:
		code = ErrorForbidden
	case response.StatusCode == http.StatusTooManyRequests:
		code = ErrorRateLimited
	case response.StatusCode >= 500 && response.StatusCode <= 599:
		code = ErrorServer
	}
	err := &ProviderError{Code: code, StatusCode: response.StatusCode, RetryAfter: safeRetryAfter(response.Header.Get("Retry-After"))}
	if response.Body != nil {
		err.BodyTruncated, err.BodyReadFailed = readBoundedErrorBody(response.Body, client.maxErrorBodyBytes)
		_ = response.Body.Close()
	}
	return err
}

func (client *Client) closeWithError(response *http.Response, err error) error {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return err
}

func readBoundedErrorBody(body io.Reader, limit int64) (truncated, failed bool) {
	if body == nil || limit <= 0 {
		return false, false
	}
	readLimit := limit
	if limit < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(body, readLimit))
	if err != nil {
		return false, true
	}
	return int64(len(data)) > limit, false
}

func endpointURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}
	if !utf8.ValidString(parsed.Path) || strings.ContainsAny(parsed.Path, "\x00\r\n") {
		return nil, providerError(ErrorInvalidConfiguration, nil)
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, providerError(ErrorInvalidConfiguration, nil)
		}
	}
	cleanPath := strings.TrimRight(parsed.Path, "/")
	if cleanPath == "" {
		cleanPath = ""
	}
	if !strings.HasSuffix(cleanPath, "/v1") {
		cleanPath += "/v1"
	}
	parsed.Path = cleanPath + "/chat/completions"
	parsed.RawPath = ""
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validCredential(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || !validHeaderValue(value, 4096) {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func validHeaderValue(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, runeValue := range value {
		if runeValue == '\r' || runeValue == '\n' || unicode.IsControl(runeValue) {
			return false
		}
	}
	return true
}

func safeRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || !validHeaderValue(value, 128) {
		return ""
	}
	if allDigits(value) {
		return value
	}
	if parsed, err := http.ParseTime(value); err == nil {
		return parsed.UTC().Format(http.TimeFormat)
	}
	return ""
}

func allDigits(value string) bool {
	for _, runeValue := range value {
		if runeValue < '0' || runeValue > '9' {
			return false
		}
	}
	return true
}

func safeMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return mediaType
}

func newDefaultHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   defaultDialTimeout,
			KeepAlive: defaultKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          defaultMaxIdleConnections,
		MaxIdleConnsPerHost:   defaultMaxIdlePerHost,
		IdleConnTimeout:       defaultIdleConnectionTimeout,
		TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
		ResponseHeaderTimeout: defaultResponseHeaderTimeout,
		ExpectContinueTimeout: defaultExpectContinueTimeout,
		DisableCompression:    true,
	}
	return &http.Client{
		Transport: transport,
		// Provider credentials must never be replayed to a redirect target. A
		// redirect is returned to Do for safe status classification instead of
		// following it, regardless of whether the target shares the host.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
