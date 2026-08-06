package codexsetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/config"
)

type Severity string

const (
	SeverityPass    Severity = "PASS"
	SeverityWarning Severity = "WARN"
	SeverityFailure Severity = "FAIL"
)

type Check struct {
	Name     string
	Severity Severity
	Message  string
}

type DoctorReport struct {
	Checks []Check
}

func (report *DoctorReport) add(severity Severity, name, message string) {
	report.Checks = append(report.Checks, Check{Name: name, Severity: severity, Message: message})
}

func (report DoctorReport) Failures() int {
	count := 0
	for _, check := range report.Checks {
		if check.Severity == SeverityFailure {
			count++
		}
	}
	return count
}

func (report DoctorReport) Warnings() int {
	count := 0
	for _, check := range report.Checks {
		if check.Severity == SeverityWarning {
			count++
		}
	}
	return count
}

func (report DoctorReport) ExitCode() int {
	if report.Failures() > 0 {
		return 1
	}
	return 0
}

type DoctorOptions struct {
	Environment Environment
	CodexHome   string
	GatewayURL  string
	HTTPClient  HTTPDoer
	LookupPath  func(string) (string, error)
	RunCommand  func(context.Context, string, ...string) ([]byte, error)
	Dial        func(context.Context, string, string) (net.Conn, error)
	Listen      func(string, string) (net.Listener, error)
	Timeout     time.Duration
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type DoctorError struct {
	Failures int
}

func (err *DoctorError) Error() string {
	return fmt.Sprintf("doctor found %d failure(s)", err.Failures)
}

func (options DoctorOptions) withDefaults() DoctorOptions {
	if options.Timeout <= 0 {
		options.Timeout = 5 * time.Second
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{
			Timeout:       options.Timeout,
			Transport:     &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if options.LookupPath == nil {
		options.LookupPath = exec.LookPath
	}
	if options.RunCommand == nil {
		options.RunCommand = runVersionCommand
	}
	if options.Dial == nil {
		dialer := &net.Dialer{Timeout: options.Timeout}
		options.Dial = dialer.DialContext
	}
	if options.Listen == nil {
		options.Listen = net.Listen
	}
	return options
}

// Diagnose performs deterministic local checks and bounded network probes.
// Every external dependency is injectable so tests do not need credentials,
// a running gateway, a DNS result, or a Codex installation.
func Diagnose(ctx context.Context, options DoctorOptions) DoctorReport {
	options = options.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	var report DoctorReport
	lookup := options.Environment.withDefaults().LookupEnv
	home, err := doctorHome(options)
	if err != nil {
		report.add(SeverityFailure, "Codex home", "could not resolve Codex home")
		return report
	}

	_, configErr := config.Load(lookup)
	if configErr != nil {
		report.add(SeverityFailure, "Gateway configuration", "gateway environment is not loadable")
	} else {
		report.add(SeverityPass, "Gateway configuration", "gateway environment is loadable")
	}
	key, keyPresent := lookup("OPENCODE_GO_API_KEY")
	if !keyPresent || strings.TrimSpace(key) == "" {
		report.add(SeverityFailure, "OpenCode Go API key", "OPENCODE_GO_API_KEY is missing (value is never printed)")
	} else if strings.ContainsAny(key, "\r\n") {
		report.add(SeverityFailure, "OpenCode Go API key", "OPENCODE_GO_API_KEY contains invalid header characters")
	} else {
		report.add(SeverityPass, "OpenCode Go API key", "credential is present (value hidden)")
	}

	configPath := filepath.Join(home, ConfigFileName)
	catalogPath := filepath.Join(home, CatalogFileName)
	configValues, configValuesOK := diagnoseCodexFiles(&report, home, configPath, catalogPath)

	gatewayURL := options.GatewayURL
	if gatewayURL == "" {
		gatewayURL = gatewayURLFromEnvironment(lookup)
	}
	gatewayURLValid := validateGatewayURL(gatewayURL) == nil
	if !gatewayURLValid {
		report.add(SeverityFailure, "Gateway provider URL", "provider URL is not a safe local HTTP or HTTPS URL")
	} else {
		report.add(SeverityPass, "Gateway provider URL", "provider URL is valid")
	}
	if configValuesOK {
		if configValues.ProviderName != "OpenCode Gateway" {
			report.add(SeverityFailure, "Codex provider name", "managed provider name is not OpenCode Gateway")
		} else {
			report.add(SeverityPass, "Codex provider name", "managed provider name is correct")
		}
		if err := validateGatewayURL(configValues.ProviderBaseURL); err != nil {
			report.add(SeverityFailure, "Codex provider URL", "configured provider URL is not safe")
		} else if options.GatewayURL == "" && configValues.ProviderBaseURL != gatewayURL {
			report.add(SeverityFailure, "Codex provider URL", "provider URL differs from the local gateway address")
		}
		if configValues.Model != ModelID || configValues.ModelProvider != ProviderID {
			report.add(SeverityFailure, "Codex model selection", "Codex is not selecting the gateway provider and deepseek-v4-flash")
		} else {
			report.add(SeverityPass, "Codex model selection", "deepseek-v4-flash is selected through opencode-gateway")
		}
		if configValues.ModelSupportsReasoningSummary || configValues.ModelReasoningEffort != "high" || configValues.ModelReasoningSummary != "none" {
			report.add(SeverityFailure, "Codex reasoning policy", "gateway v0.1 requires high effort without reasoning summaries")
		} else {
			report.add(SeverityPass, "Codex reasoning policy", "reasoning summaries are disabled")
		}
		if options.GatewayURL != "" && configValues.ProviderBaseURL != options.GatewayURL {
			report.add(SeverityFailure, "Gateway provider URL", "Codex provider URL does not match the requested gateway URL")
		}
		if configValues.ProviderWireAPI != "responses" {
			report.add(SeverityFailure, "Codex wire API", "gateway provider must use the Responses wire API")
		} else {
			report.add(SeverityPass, "Codex wire API", "Responses wire API is configured")
		}
		if configValues.ProviderSupportsWebsockets || configValues.RequestMaxRetries != 0 || configValues.StreamMaxRetries != 0 {
			report.add(SeverityFailure, "Codex transport policy", "WebSockets and provider retries must be disabled for gateway v0.1")
		} else {
			report.add(SeverityPass, "Codex transport policy", "WebSockets and provider retries are disabled")
		}
	}

	if gatewayURLValid {
		diagnoseGateway(ctx, &report, options, gatewayURL)
	}
	diagnoseProvider(ctx, &report, options, lookup, key, keyPresent)
	diagnoseCodexExecutable(ctx, &report, options)
	return report
}

func doctorHome(options DoctorOptions) (string, error) {
	if options.CodexHome != "" {
		if !filepath.IsAbs(options.CodexHome) {
			return "", errors.New("Codex home is not absolute")
		}
		return filepath.Clean(options.CodexHome), nil
	}
	return ResolveCodexHome(options.Environment)
}

func diagnoseCodexFiles(report *DoctorReport, home, configPath, defaultCatalogPath string) (ConfigValues, bool) {
	configInfo, err := os.Stat(configPath)
	if errors.Is(err, os.ErrNotExist) {
		report.add(SeverityFailure, "Codex config", "config.toml does not exist")
		return ConfigValues{}, false
	}
	if err != nil || !configInfo.Mode().IsRegular() {
		report.add(SeverityFailure, "Codex config", "config.toml is not a readable regular file")
		return ConfigValues{}, false
	}
	diagnosePermissions(report, "Codex config permissions", configInfo.Mode())
	configData, err := os.ReadFile(configPath)
	if err != nil {
		report.add(SeverityFailure, "Codex config", "config.toml could not be read")
		return ConfigValues{}, false
	}
	values, err := InspectConfig(configData)
	if err != nil {
		report.add(SeverityFailure, "Codex config", "config.toml failed validation")
		return ConfigValues{}, false
	}
	report.add(SeverityPass, "Codex config", "config.toml parses and contains the managed provider")
	catalogPath, err := resolveCatalogPath(home, values.ModelCatalogJSON)
	if err != nil {
		report.add(SeverityFailure, "Model catalog path", "model_catalog_json is not a safe path")
		return values, true
	}
	if catalogPath == "" {
		catalogPath = defaultCatalogPath
	}
	catalogInfo, err := os.Stat(catalogPath)
	if errors.Is(err, os.ErrNotExist) {
		report.add(SeverityFailure, "Model catalog", "models.json does not exist")
		return values, true
	}
	if err != nil || !catalogInfo.Mode().IsRegular() {
		report.add(SeverityFailure, "Model catalog", "models.json is not a readable regular file")
		return values, true
	}
	diagnosePermissions(report, "Model catalog permissions", catalogInfo.Mode())
	catalogData, err := os.ReadFile(catalogPath)
	if err != nil {
		report.add(SeverityFailure, "Model catalog", "models.json could not be read")
		return values, true
	}
	if _, err := ValidateCatalog(catalogData); err != nil {
		report.add(SeverityFailure, "Model catalog", "models.json failed validation or does not contain deepseek-v4-flash")
	} else {
		report.add(SeverityPass, "Model catalog", "models.json contains deepseek-v4-flash metadata")
	}
	return values, true
}

func resolveCatalogPath(home, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "~/") || value == "~" {
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	return resolvePathWithinHome(home, value)
}

func diagnosePermissions(report *DoctorReport, name string, mode os.FileMode) {
	if mode.Perm()&0o022 != 0 {
		report.add(SeverityFailure, name, "managed file is writable by group or other users")
		return
	}
	if mode.Perm()&0o077 != 0 {
		report.add(SeverityWarning, name, "managed file is readable beyond the owner")
		return
	}
	report.add(SeverityPass, name, "managed file permissions are restrictive")
}

func gatewayURLFromEnvironment(lookup func(string) (string, bool)) string {
	host := config.DefaultHost
	if value, ok := lookup("OPENCODE_GATEWAY_HOST"); ok && value != "" {
		host = value
	}
	port := config.DefaultPort
	if value, ok := lookup("OPENCODE_GATEWAY_PORT"); ok && value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 && parsed <= 65535 {
			port = parsed
		}
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/v1"
}

func diagnoseGateway(ctx context.Context, report *DoctorReport, options DoctorOptions, providerURL string) {
	parsed, err := url.Parse(providerURL)
	if err != nil || parsed.Host == "" {
		report.add(SeverityFailure, "Gateway port", "gateway address is invalid")
		return
	}
	address := parsed.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		if strings.Contains(address, ":") && !strings.HasPrefix(address, "[") {
			address = "[" + address + "]"
		}
		if parsed.Port() == "" {
			report.add(SeverityFailure, "Gateway port", "gateway address has no port")
			return
		}
	}
	dialContext, cancel := context.WithTimeout(ctx, options.Timeout)
	connection, dialErr := options.Dial(dialContext, "tcp", parsed.Host)
	cancel()
	if dialErr != nil {
		listener, listenErr := options.Listen("tcp", parsed.Host)
		if listenErr == nil {
			_ = listener.Close()
			report.add(SeverityWarning, "Gateway port", "gateway port is available but no gateway listener is running")
		} else {
			report.add(SeverityFailure, "Gateway port", "gateway port is unavailable")
		}
	} else {
		_ = connection.Close()
		report.add(SeverityPass, "Gateway port", "gateway port is accepting connections")
	}

	base := strings.TrimSuffix(providerURL, "/v1")
	for _, endpoint := range []string{"/health/live", "/health/ready"} {
		status, ok := doStatusCheck(ctx, options.HTTPClient, strings.TrimRight(base, "/")+endpoint, http.MethodGet)
		name := "Gateway " + strings.TrimPrefix(endpoint, "/health/")
		if !ok {
			report.add(SeverityFailure, name, "gateway health endpoint is unavailable")
			continue
		}
		if status != http.StatusOK {
			report.add(SeverityFailure, name, "gateway health endpoint returned an unexpected status")
			continue
		}
		report.add(SeverityPass, name, "gateway health endpoint is healthy")
	}
}

func doStatusCheck(ctx context.Context, client HTTPDoer, endpoint, method string) (int, bool) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return 0, false
	}
	response, err := client.Do(request)
	if err != nil || response == nil {
		return 0, false
	}
	if response.Body != nil {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	}
	return response.StatusCode, true
}

func diagnoseProvider(ctx context.Context, report *DoctorReport, options DoctorOptions, lookup func(string) (string, bool), key string, keyPresent bool) {
	baseURL := config.DefaultBaseURL
	if value, ok := lookup("OPENCODE_GO_BASE_URL"); ok && value != "" {
		baseURL = value
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		report.add(SeverityFailure, "OpenCode Go endpoint", "configured provider endpoint is invalid")
		return
	}
	if err := validateGatewayURL(baseURL); err != nil {
		report.add(SeverityFailure, "OpenCode Go endpoint", "configured provider endpoint is not a safe HTTP or HTTPS URL")
		return
	}
	if !keyPresent || strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
		report.add(SeverityWarning, "OpenCode Go models", "skipped authenticated provider check because the key is unavailable")
		return
	}
	endpoint := *parsed
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		report.add(SeverityFailure, "OpenCode Go models", "could not create provider validation request")
		return
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	response, err := options.HTTPClient.Do(request)
	if err != nil || response == nil {
		report.add(SeverityFailure, "OpenCode Go connectivity", "provider host could not be reached")
		return
	}
	var body []byte
	if response.Body != nil {
		defer response.Body.Close()
		body, err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			report.add(SeverityFailure, "OpenCode Go models", "provider model response could not be read")
			return
		}
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		report.add(SeverityFailure, "OpenCode Go authentication", "provider rejected the configured credential")
		return
	case response.StatusCode == http.StatusTooManyRequests:
		report.add(SeverityWarning, "OpenCode Go authentication", "provider rate-limited the safe model check")
		return
	case response.StatusCode >= 500:
		report.add(SeverityFailure, "OpenCode Go connectivity", "provider returned a server failure")
		return
	case response.StatusCode < 200 || response.StatusCode >= 300:
		report.add(SeverityFailure, "OpenCode Go models", "provider returned an unexpected status")
		return
	}
	report.add(SeverityPass, "OpenCode Go authentication", "provider accepted the credential for a models check")
	if providerModelsContain(body, ModelID) {
		report.add(SeverityPass, "OpenCode Go model", "deepseek-v4-flash is available")
	} else {
		report.add(SeverityFailure, "OpenCode Go model", "deepseek-v4-flash was not returned by the provider")
	}
}

func providerModelsContain(data []byte, wanted string) bool {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}
	for _, model := range append(payload.Data, payload.Models...) {
		if model.ID == wanted {
			return true
		}
	}
	return false
}

func diagnoseCodexExecutable(ctx context.Context, report *DoctorReport, options DoctorOptions) {
	path, err := options.LookupPath("codex")
	if err != nil || path == "" {
		report.add(SeverityWarning, "Codex executable", "codex was not found on PATH")
		return
	}
	output, err := options.RunCommand(ctx, path, "--version")
	if err != nil {
		report.add(SeverityWarning, "Codex executable", "codex was found but --version failed")
		return
	}
	if !codexVersionPattern.Match(output) {
		report.add(SeverityWarning, "Codex executable", "codex version output could not be recognized")
		return
	}
	report.add(SeverityPass, "Codex executable", "codex is installed and reports a version")
}

var codexVersionPattern = regexp.MustCompile(`\b\d+\.\d+(?:\.\d+)?\b`)

func runVersionCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, args...)
	return command.Output()
}
