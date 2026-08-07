package codexsetup

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveCodexHomeUsesExplicitHomeOrUserLocation(t *testing.T) {
	userHome := filepath.Join(t.TempDir(), "home with spaces", "日本語")
	environment := Environment{UserHomeDir: func() (string, error) { return userHome, nil }, LookupEnv: func(string) (string, bool) { return "", false }}
	got, err := ResolveCodexHome(environment)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(userHome, ".codex"); got != want {
		t.Fatalf("default Codex home = %q, want %q", got, want)
	}

	explicit := filepath.Join(t.TempDir(), "codex")
	environment.LookupEnv = func(key string) (string, bool) {
		if key == "CODEX_HOME" {
			return explicit, true
		}
		return "", false
	}
	got, err = ResolveCodexHome(environment)
	if err != nil || got != explicit {
		t.Fatalf("explicit Codex home = %q, err=%v, want %q", got, err, explicit)
	}

	for _, value := range []string{"relative", "   "} {
		environment.LookupEnv = func(string) (string, bool) { return value, true }
		if _, err := ResolveCodexHome(environment); err == nil {
			t.Fatalf("ResolveCodexHome(%q) succeeded", value)
		}
	}
}

func TestSetupCodexCreatesSecureIdempotentFilesAndBackup(t *testing.T) {
	home := t.TempDir()
	fixedTime := time.Date(2026, time.August, 6, 20, 0, 0, 0, time.UTC)
	result, err := SetupCodex(SetupOptions{CodexHome: home, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.BackupPath == "" {
		t.Fatalf("setup result = %+v, want changed setup with backup", result)
	}
	for _, path := range []string{result.ConfigPath, result.CatalogPath, result.ProfilePath, result.ZenFreeProfilePath, result.AgentPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != managedFileMode.Perm() {
			t.Fatalf("%s mode = %o, want %o", filepath.Base(path), info.Mode().Perm(), managedFileMode.Perm())
		}
	}
	configData, err := os.ReadFile(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := InspectProvider(configData)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name != "OpenCode Gateway (Go)" || provider.BaseURL != DefaultGoGatewayURL || provider.WireAPI != "responses" || provider.SupportsWebsockets || provider.RequestMaxRetries != 0 || provider.StreamMaxRetries != 0 {
		t.Fatalf("managed Go provider values = %+v", provider)
	}
	zenProvider, err := InspectProviderFor(configData, ZenProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if zenProvider.Name != "OpenCode Gateway (Zen)" || zenProvider.BaseURL != DefaultZenGatewayURL || zenProvider.WireAPI != "responses" || zenProvider.SupportsWebsockets || zenProvider.RequestMaxRetries != 0 || zenProvider.StreamMaxRetries != 0 {
		t.Fatalf("managed Zen provider values = %+v", zenProvider)
	}
	if strings.Contains(string(configData), "model_provider =") {
		t.Fatalf("default config still routes a root model_provider:\n%s", configData)
	}
	profileData, err := os.ReadFile(result.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	values, err := InspectProfile(profileData)
	if err != nil {
		t.Fatal(err)
	}
	if values.Model != GoModelID || values.ModelProvider != GoProviderID || values.ModelCatalogJSON != filepath.Join(home, CatalogFileName) {
		t.Fatalf("profile values = %+v", values)
	}
	zenFreeProfileData, err := os.ReadFile(result.ZenFreeProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	zenFreeValues, err := InspectProfile(zenFreeProfileData)
	if err != nil {
		t.Fatal(err)
	}
	if zenFreeValues.Model != ZenFreeModelID || zenFreeValues.ModelProvider != ZenProviderID || zenFreeValues.ModelCatalogJSON != filepath.Join(home, CatalogFileName) {
		t.Fatalf("Zen Free profile values = %+v", zenFreeValues)
	}
	agentData, err := os.ReadFile(result.AgentPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateSubagentData(agentData); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(agentData), subagentName) == false {
		t.Fatalf("subagent file does not define %s:\n%s", subagentName, agentData)
	}
	catalogData, err := os.ReadFile(result.CatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCatalog(catalogData); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{string(configData), string(catalogData), string(profileData), string(agentData)} {
		if strings.Contains(content, "OPENCODE_GO_API_KEY") {
			t.Fatal("generated Codex files mention the gateway credential")
		}
	}

	repeated, err := SetupCodex(SetupOptions{CodexHome: home, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Changed || repeated.BackupPath != "" || repeated.Diff != "no changes" {
		t.Fatalf("repeated setup = %+v, want no changes", repeated)
	}
	for _, providerID := range []string{GoProviderID, ZenProviderID} {
		if got := strings.Count(string(configData), "[model_providers."+providerID+"]"); got != 1 {
			t.Fatalf("provider table %s count = %d, want 1", providerID, got)
		}
	}
	if _, err := os.Stat(filepath.Join(result.BackupPath, "manifest.json")); err != nil {
		t.Fatalf("backup manifest: %v", err)
	}
}

func TestSetupCodexPreservesUnrelatedTomlAndRemovesGatewayCredentialFields(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)
	original := `# preserve this comment
unrelated = "keep me"

[[mcp_servers]]
name = "array table remains"

[model_providers.openai]
name = "built-in remains untouched"

[model_providers.opencode-gateway-go]
# retain this provider comment
name = "old name" # retain inline comment
base_url = "http://127.0.0.1:9999/v1"
experimental_bearer_token = "do-not-copy"
custom_setting = "preserve"
`
	if err := os.WriteFile(configPath, []byte(original), managedFileMode); err != nil {
		t.Fatal(err)
	}
	result, err := SetupCodex(SetupOptions{CodexHome: home, GatewayURL: "http://127.0.0.1:9998/v1"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, expected := range []string{"# preserve this comment", "unrelated = \"keep me\"", "[[mcp_servers]]", "name = \"array table remains\"", "name = \"built-in remains untouched\"", "# retain this provider comment", "custom_setting = \"preserve\""} {
		if !strings.Contains(content, expected) {
			t.Fatalf("updated config lost %q:\n%s", expected, content)
		}
	}
	if strings.Contains(content, "do-not-copy") || strings.Contains(content, "experimental_bearer_token") {
		t.Fatalf("gateway credential field survived setup:\n%s", content)
	}
	for _, providerID := range []string{GoProviderID, ZenProviderID} {
		if got := strings.Count(content, "[model_providers."+providerID+"]"); got != 1 {
			t.Fatalf("provider table %s count = %d, want 1", providerID, got)
		}
	}
	if got := strings.Count(content, "model_provider ="); got != 0 {
		t.Fatalf("gateway routing was introduced into the default config: %d assignment(s)", got)
	}
}

func TestSetupCodexRemovesLegacySingleProviderTable(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)
	legacy := `[model_providers.opencode-gateway]
name = "old name"
base_url = "http://127.0.0.1:8787/v1"
experimental_bearer_token = "do-not-copy"
custom_setting = "preserve"

[model_providers.openai]
name = "built-in remains untouched"
`
	if err := os.WriteFile(configPath, []byte(legacy), managedFileMode); err != nil {
		t.Fatal(err)
	}
	result, err := SetupCodex(SetupOptions{CodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "[model_providers."+LegacyProviderID+"]") || strings.Contains(content, "do-not-copy") || strings.Contains(content, "experimental_bearer_token") {
		t.Fatalf("legacy provider table or credential survived setup:\n%s", content)
	}
	if !strings.Contains(content, "name = \"built-in remains untouched\"") {
		t.Fatalf("unrelated provider table was lost:\n%s", content)
	}
}

func TestSetupCodexStripsStaleDefaultGatewayRouting(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)
	stale := `unrelated = "keep me"

model = "deepseek-v4-flash"
model_provider = "opencode-gateway"
model_catalog_json = "` + filepath.Join(home, CatalogFileName) + `"
model_reasoning_effort = "high"
model_supports_reasoning_summaries = false
model_reasoning_summary = "none"

[model_providers.opencode-gateway]
name = "old"
base_url = "http://127.0.0.1:9999/v1"
`
	if err := os.WriteFile(configPath, []byte(stale), managedFileMode); err != nil {
		t.Fatal(err)
	}
	result, err := SetupCodex(SetupOptions{CodexHome: home, GatewayURL: "http://127.0.0.1:9998/v1"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "unrelated = \"keep me\"") {
		t.Fatalf("stale config lost unrelated source:\n%s", content)
	}
	for _, staleKey := range []string{"model =", "model_provider =", "model_catalog_json =", "model_reasoning_effort =", "model_supports_reasoning_summaries =", "model_reasoning_summary ="} {
		if strings.Contains(content, staleKey) {
			t.Fatalf("stale gateway routing key %q survived setup:\n%s", staleKey, content)
		}
	}
	if strings.Contains(content, "[model_providers."+LegacyProviderID+"]") {
		t.Fatalf("legacy provider table survived setup:\n%s", content)
	}
	profileData, err := os.ReadFile(result.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectProfile(profileData); err != nil {
		t.Fatalf("profile did not receive the migrated gateway routing: %v", err)
	}
}

func TestSetupCodexPreservesCustomizedDefaultRouting(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)
	custom := "model = \"gpt-5.5\"\nmodel_provider = \"openai\"\n"
	if err := os.WriteFile(configPath, []byte(custom), managedFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := SetupCodex(SetupOptions{CodexHome: home}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "model = \"gpt-5.5\"") || !strings.Contains(content, "model_provider = \"openai\"") {
		t.Fatalf("user-customized default routing was overwritten:\n%s", content)
	}
}

func TestSetupCodexPreservesMultilineCodexCollections(t *testing.T) {
	home := t.TempDir()
	original := `[tool_suggest]
discoverables = [
  { type = "connector", id = "github" }
]

[apps.github]
enabled = true
`
	if err := os.WriteFile(filepath.Join(home, ConfigFileName), []byte(original), managedFileMode); err != nil {
		t.Fatal(err)
	}

	result, err := SetupCodex(SetupOptions{CodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, original) {
		t.Fatalf("multiline Codex collection was not preserved:\n%s", content)
	}

	repeated, err := SetupCodex(SetupOptions{CodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Changed {
		t.Fatalf("setup after preserving multiline collection changed config: %+v", repeated)
	}
}

func TestSetupCodexRejectsUnclosedMultilineCollectionWithoutWrites(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)
	malformed := `[tool_suggest]
discoverables = [
  { type = "connector", id = "github" }
`
	if err := os.WriteFile(configPath, []byte(malformed), managedFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := SetupCodex(SetupOptions{CodexHome: home}); err == nil {
		t.Fatal("unclosed multiline collection was accepted")
	}
	if _, err := os.Stat(filepath.Join(home, CatalogFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malformed setup created catalog: %v", err)
	}
}

func TestSetupCodexDryRunDoesNotWriteOrRevealSecrets(t *testing.T) {
	home := t.TempDir()
	secret := "super-secret-api-key"
	configPath := filepath.Join(home, ConfigFileName)
	original := "existing = \"" + secret + "\"\n"
	if err := os.WriteFile(configPath, []byte(original), managedFileMode); err != nil {
		t.Fatal(err)
	}
	result, err := SetupCodex(SetupOptions{CodexHome: home, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Diff, "redacted") || strings.Contains(result.Diff, secret) {
		t.Fatalf("dry-run diff = %q", result.Diff)
	}
	if got, err := os.ReadFile(configPath); err != nil || string(got) != original {
		t.Fatalf("dry-run changed config: %q, err=%v", got, err)
	}
	for _, absent := range []string{CatalogFileName, GoProfileFileName, ZenFreeProfileFileName, filepath.Join(AgentsDirName, SubagentFileName)} {
		if _, err := os.Stat(filepath.Join(home, absent)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry-run created %s: %v", absent, err)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), BackupPrefix) {
			t.Fatal("dry-run created a backup")
		}
	}
}

func TestSetupCodexRejectsMalformedConfigWithoutPartialWrites(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)
	if err := os.WriteFile(configPath, []byte("model = \"unterminated\n"), managedFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := SetupCodex(SetupOptions{CodexHome: home}); err == nil {
		t.Fatal("malformed config was accepted")
	}
	if _, err := os.Stat(filepath.Join(home, CatalogFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malformed setup created catalog: %v", err)
	}
	if entries, err := os.ReadDir(home); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 || entries[0].Name() != ConfigFileName {
		t.Fatalf("malformed setup changed home: %+v", entries)
	}
}

func TestReplaceFilesRollsBackWhenSecondAtomicRenameFails(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)
	catalogPath := filepath.Join(home, CatalogFileName)
	if err := os.WriteFile(configPath, []byte("old config\n"), managedFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, []byte("old catalog\n"), managedFileMode); err != nil {
		t.Fatal(err)
	}
	previousConfig, err := readFileState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	previousCatalog, err := readFileState(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	next := []fileState{
		{path: configPath, data: []byte("new config\n"), mode: managedFileMode, exists: true},
		{path: catalogPath, data: []byte("new catalog\n"), mode: managedFileMode, exists: true},
	}
	previous := []fileState{previousConfig, previousCatalog}
	renameCalls := 0
	errInjected := errors.New("simulated interrupted rename")
	err = replaceFilesWith(next, previous, func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 2 {
			return errInjected
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil || !strings.Contains(err.Error(), errInjected.Error()) {
		t.Fatalf("replaceFilesWith error = %v", err)
	}
	for _, test := range []struct {
		path string
		want string
	}{
		{path: configPath, want: "old config\n"},
		{path: catalogPath, want: "old catalog\n"},
	} {
		got, readErr := os.ReadFile(test.path)
		if readErr != nil || string(got) != test.want {
			t.Fatalf("%s after rollback = %q, err=%v", filepath.Base(test.path), got, readErr)
		}
	}
}

func TestSetupCodexSupportsUnicodeHomeAndRestore(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Codex 用户 home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("# user setting\ntrusted = true\n")
	if err := os.WriteFile(filepath.Join(home, ConfigFileName), original, managedFileMode); err != nil {
		t.Fatal(err)
	}
	result, err := SetupCodex(SetupOptions{CodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.CatalogPath, "用户 home") {
		t.Fatalf("catalog path lost Unicode home: %q", result.CatalogPath)
	}
	if err := os.WriteFile(result.ConfigPath, []byte("model = \"changed\"\n"), managedFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(Environment{}, home, result.BackupPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("restored config = %q, want %q", got, original)
	}
	if _, err := os.Stat(filepath.Join(home, CatalogFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore retained generated catalog: %v", err)
	}
	for _, absent := range []string{GoProfileFileName, ZenFreeProfileFileName, filepath.Join(AgentsDirName, SubagentFileName)} {
		if _, err := os.Stat(filepath.Join(home, absent)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restore retained generated %s: %v", absent, err)
		}
	}
}

func TestValidateCatalogRejectsMissingModelAndAcceptsGeneratedCatalog(t *testing.T) {
	data, err := GenerateCatalog()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ValidateCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 || catalog.Models[0].BaseInstructions == "" || catalog.Models[0].ExperimentalSupportedTools == nil {
		t.Fatal("generated catalog is missing the base instructions or experimental tools field")
	}
	if _, err := ValidateCatalog([]byte(`{"models":[{"slug":"other","display_name":"Other"}]}`)); err == nil {
		t.Fatal("catalog without gateway model was accepted")
	}
	if _, err := ValidateCatalog([]byte(`{"models":`)); err == nil {
		t.Fatal("malformed catalog was accepted")
	}
	if _, err := ValidateCatalog([]byte(`{"models":[{"slug":"other","display_name":"Other","base_instructions":"x"}]}`)); err == nil {
		t.Fatal("catalog without gateway model was accepted")
	}
	if _, err := ValidateCatalog([]byte(`{"models":[{"slug":"deepseek-v4-flash","display_name":"Flash"}]}`)); err == nil {
		t.Fatal("catalog entry without base instructions was accepted")
	}
}

type responseDoer func(*http.Request) (*http.Response, error)

func (doer responseDoer) Do(request *http.Request) (*http.Response, error) {
	return doer(request)
}

func doctorResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func doctorEnvironment(key string) Environment {
	return Environment{LookupEnv: func(name string) (string, bool) {
		switch name {
		case "OPENCODE_GO_API_KEY":
			return key, key != ""
		case "OPENCODE_GO_BASE_URL":
			return "https://provider.example/v1", true
		default:
			return "", false
		}
	}}
}

func successfulDoctorOptions(home string, providerStatus int, models string) DoctorOptions {
	return DoctorOptions{
		Environment: doctorEnvironment("secret-key-not-output"),
		CodexHome:   home,
		HTTPClient: responseDoer(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "/models") {
				return doctorResponse(providerStatus, models), nil
			}
			return doctorResponse(http.StatusOK, `{"status":"ok"}`), nil
		}),
		LookupPath: func(string) (string, error) { return "/usr/bin/codex", nil },
		RunCommand: func(context.Context, string, ...string) ([]byte, error) { return []byte("codex 0.146.0"), nil },
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, peer := net.Pipe()
			_ = peer.Close()
			return client, nil
		},
	}
}

func TestDiagnosePassesDeterministicHealthySetup(t *testing.T) {
	home := t.TempDir()
	if _, err := SetupCodex(SetupOptions{CodexHome: home}); err != nil {
		t.Fatal(err)
	}
	report := Diagnose(context.Background(), successfulDoctorOptions(home, http.StatusOK, `{"data":[{"id":"deepseek-v4-flash"}]}`))
	if report.Failures() != 0 {
		t.Fatalf("healthy doctor report has failures: %+v", report.Checks)
	}
}

func TestDiagnoseDetectsMissingKeyBadCatalogAndProviderStatus(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		home := t.TempDir()
		if _, err := SetupCodex(SetupOptions{CodexHome: home}); err != nil {
			t.Fatal(err)
		}
		options := successfulDoctorOptions(home, http.StatusOK, `{"data":[{"id":"deepseek-v4-flash"}]}`)
		options.Environment = doctorEnvironment("")
		report := Diagnose(context.Background(), options)
		if report.Failures() == 0 || !hasCheckMessage(report, "OpenCode Go API key", "missing") {
			t.Fatalf("missing-key report = %+v", report.Checks)
		}
		if hasCheckMessage(report, "OpenCode Go API key", "secret-key-not-output") {
			t.Fatal("doctor report exposed a credential")
		}
	})

	t.Run("bad catalog", func(t *testing.T) {
		home := t.TempDir()
		if _, err := SetupCodex(SetupOptions{CodexHome: home}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, CatalogFileName), []byte(`{"models":[]}`), managedFileMode); err != nil {
			t.Fatal(err)
		}
		report := Diagnose(context.Background(), successfulDoctorOptions(home, http.StatusOK, `{"data":[{"id":"deepseek-v4-flash"}]}`))
		if !hasCheck(report, "Model catalog", SeverityFailure) {
			t.Fatalf("bad catalog report = %+v", report.Checks)
		}
	})

	for _, test := range []struct {
		name         string
		status       int
		wantSeverity Severity
		checkName    string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantSeverity: SeverityFailure, checkName: "OpenCode Go authentication"},
		{name: "rate limited", status: http.StatusTooManyRequests, wantSeverity: SeverityWarning, checkName: "OpenCode Go authentication"},
		{name: "server error", status: http.StatusBadGateway, wantSeverity: SeverityFailure, checkName: "OpenCode Go connectivity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if _, err := SetupCodex(SetupOptions{CodexHome: home}); err != nil {
				t.Fatal(err)
			}
			report := Diagnose(context.Background(), successfulDoctorOptions(home, test.status, `{}`))
			if !hasCheck(report, test.checkName, test.wantSeverity) {
				t.Fatalf("%s report = %+v", test.name, report.Checks)
			}
		})
	}
}

func TestDiagnoseDetectsMissingModelAndCodexWarning(t *testing.T) {
	home := t.TempDir()
	if _, err := SetupCodex(SetupOptions{CodexHome: home}); err != nil {
		t.Fatal(err)
	}
	options := successfulDoctorOptions(home, http.StatusOK, `{"data":[{"id":"other-model"}]}`)
	options.LookupPath = func(string) (string, error) { return "", errors.New("not found") }
	report := Diagnose(context.Background(), options)
	if !hasCheck(report, "OpenCode Go model", SeverityFailure) || !hasCheck(report, "Codex executable", SeverityWarning) {
		t.Fatalf("missing model/codex report = %+v", report.Checks)
	}
}

func TestDiagnoseDoesNotProbeInvalidEndpoints(t *testing.T) {
	var probes int
	options := DoctorOptions{
		Environment: Environment{LookupEnv: func(name string) (string, bool) {
			switch name {
			case "OPENCODE_GO_API_KEY":
				return "test-key", true
			case "OPENCODE_GO_BASE_URL":
				return "http://provider.example/v1", true
			default:
				return "", false
			}
		}},
		CodexHome:  t.TempDir(),
		GatewayURL: "http://gateway.example:8787/v1",
		HTTPClient: responseDoer(func(*http.Request) (*http.Response, error) {
			probes++
			return doctorResponse(http.StatusOK, `{}`), nil
		}),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			probes++
			return nil, errors.New("unexpected dial")
		},
		LookupPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	report := Diagnose(context.Background(), options)
	if probes != 0 {
		t.Fatalf("invalid endpoints triggered %d network probes", probes)
	}
	if !hasCheck(report, "Gateway provider URL", SeverityFailure) {
		t.Fatalf("invalid gateway URL report = %+v", report.Checks)
	}
	if !hasCheck(report, "OpenCode Go endpoint", SeverityFailure) {
		t.Fatalf("invalid provider URL report = %+v", report.Checks)
	}
}

func hasCheck(report DoctorReport, name string, severity Severity) bool {
	for _, check := range report.Checks {
		if check.Name == name && check.Severity == severity {
			return true
		}
	}
	return false
}

func hasCheckMessage(report DoctorReport, name, fragment string) bool {
	for _, check := range report.Checks {
		if check.Name == name && strings.Contains(check.Message, fragment) {
			return true
		}
	}
	return false
}
