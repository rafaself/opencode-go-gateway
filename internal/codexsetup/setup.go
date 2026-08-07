package codexsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const managedFileMode os.FileMode = 0o600

type SetupOptions struct {
	Environment   Environment
	CodexHome     string
	GatewayURL    string
	ZenGatewayURL string
	DryRun        bool
	Now           func() time.Time
}

type SetupResult struct {
	CodexHome          string
	ConfigPath         string
	CatalogPath        string
	ProfilePath        string
	ZenFreeProfilePath string
	AgentPath          string
	BackupPath         string
	Changed            bool
	Diff               string
}

type fileState struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

type backupManifest struct {
	Version int          `json:"version"`
	Files   []backupFile `json:"files"`
}

type backupFile struct {
	Name   string      `json:"name"`
	Exists bool        `json:"exists"`
	Mode   os.FileMode `json:"mode"`
}

type tomlSetting struct {
	Key   string
	Value string
}

// goProviderSettings and zenProviderSettings are the managed provider tables
// written into the user's config.toml. They differ only in name and base URL,
// which is injected per run. The Zen table is the single Zen backend: free
// models are selected per profile through the shared table.
var goProviderSettings = []tomlSetting{
	{Key: "name", Value: `"OpenCode Gateway (Go)"`},
	{Key: "base_url", Value: ""},
	{Key: "wire_api", Value: `"responses"`},
	{Key: "supports_websockets", Value: "false"},
	{Key: "request_max_retries", Value: "0"},
	{Key: "stream_max_retries", Value: "0"},
}

var zenProviderSettings = []tomlSetting{
	{Key: "name", Value: `"OpenCode Gateway (Zen)"`},
	{Key: "base_url", Value: ""},
	{Key: "wire_api", Value: `"responses"`},
	{Key: "supports_websockets", Value: "false"},
	{Key: "request_max_retries", Value: "0"},
	{Key: "stream_max_retries", Value: "0"},
}

// previousReasoningSettings are the root reasoning keys written by the first
// setup revision, which routed the default Codex session through the gateway.
// They are removed when the gateway session moves into the profile.
var previousReasoningSettings = []tomlSetting{
	{Key: "model_reasoning_effort", Value: `"high"`},
	{Key: "model_supports_reasoning_summaries", Value: "false"},
	{Key: "model_reasoning_summary", Value: `"none"`},
}

var forbiddenProviderKeys = map[string]bool{
	"experimental_bearer_token": true,
	"env_key":                   true,
	"env_key_instructions":      true,
}

func (options SetupOptions) withDefaults() SetupOptions {
	if options.GatewayURL == "" {
		options.GatewayURL = DefaultGoGatewayURL
	}
	if options.ZenGatewayURL == "" {
		options.ZenGatewayURL = DefaultZenGatewayURL
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return options
}

// SetupCodex creates or updates the user-level Codex provider artifacts: the
// managed Go and Zen provider tables in config.toml, the two gateway session
// profiles, the deepseek subagent definition, and the model catalog. The
// default Codex session is left on Codex's built-in models; the gateway is
// opt-in via `codex --profile opencode-gateway-go` or `codex --profile
// opencode-gateway-zen-free`, or a spawned subagent. Existing source lines
// are retained except for managed keys, and all target files are staged and
// validated before any target replacement occurs.
func SetupCodex(options SetupOptions) (SetupResult, error) {
	options = options.withDefaults()
	home, err := setupHome(options)
	if err != nil {
		return SetupResult{}, err
	}
	if err := validateGatewayURL(options.GatewayURL); err != nil {
		return SetupResult{}, err
	}
	if err := validateGatewayURL(options.ZenGatewayURL); err != nil {
		return SetupResult{}, err
	}
	configPath := filepath.Join(home, ConfigFileName)
	catalogPath := filepath.Join(home, CatalogFileName)
	profilePath := filepath.Join(home, GoProfileFileName)
	zenFreeProfilePath := filepath.Join(home, ZenFreeProfileFileName)
	agentDir := filepath.Join(home, AgentsDirName)
	agentPath := filepath.Join(agentDir, SubagentFileName)
	configState, err := readFileState(configPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read Codex config: %w", err)
	}
	catalogState, err := readFileState(catalogPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read model catalog: %w", err)
	}
	profileState, err := readFileState(profilePath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read gateway profile: %w", err)
	}
	zenFreeProfileState, err := readFileState(zenFreeProfilePath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read Zen Free gateway profile: %w", err)
	}
	agentState, err := readFileState(agentPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read gateway subagent: %w", err)
	}

	configDocument, err := parseTOML(string(configState.data))
	if err != nil {
		return SetupResult{}, fmt.Errorf("validate existing Codex config: %w", err)
	}
	configData, err := renderConfig(configDocument, home, options.GatewayURL, options.ZenGatewayURL)
	if err != nil {
		return SetupResult{}, err
	}
	if _, err := parseTOML(string(configData)); err != nil {
		return SetupResult{}, fmt.Errorf("validate generated Codex config: %w", err)
	}
	catalogData, err := GenerateCatalog()
	if err != nil {
		return SetupResult{}, err
	}
	if _, err := ValidateCatalog(catalogData); err != nil {
		return SetupResult{}, fmt.Errorf("validate generated model catalog: %w", err)
	}
	profileData, err := renderProfile(home, GoProfileFileName, GoProviderID, GoModelID)
	if err != nil {
		return SetupResult{}, err
	}
	if _, err := InspectProfile(profileData); err != nil {
		return SetupResult{}, fmt.Errorf("validate generated Codex profile: %w", err)
	}
	zenFreeProfileData, err := renderProfile(home, ZenFreeProfileFileName, ZenProviderID, ZenFreeModelID)
	if err != nil {
		return SetupResult{}, err
	}
	if _, err := InspectProfile(zenFreeProfileData); err != nil {
		return SetupResult{}, fmt.Errorf("validate generated Zen Free Codex profile: %w", err)
	}
	agentData, err := renderSubagent()
	if err != nil {
		return SetupResult{}, err
	}
	if _, err := validateSubagentData(agentData); err != nil {
		return SetupResult{}, fmt.Errorf("validate generated Codex subagent: %w", err)
	}

	configMode := secureFileMode(configState)
	catalogMode := secureFileMode(catalogState)
	profileMode := secureFileMode(profileState)
	zenFreeProfileMode := secureFileMode(zenFreeProfileState)
	agentMode := secureFileMode(agentState)
	configChanged := !bytes.Equal(configState.data, configData) || configState.mode.Perm() != configMode.Perm() || !configState.exists
	catalogChanged := !bytes.Equal(catalogState.data, catalogData) || catalogState.mode.Perm() != catalogMode.Perm() || !catalogState.exists
	profileChanged := !bytes.Equal(profileState.data, profileData) || profileState.mode.Perm() != profileMode.Perm() || !profileState.exists
	zenFreeProfileChanged := !bytes.Equal(zenFreeProfileState.data, zenFreeProfileData) || zenFreeProfileState.mode.Perm() != zenFreeProfileMode.Perm() || !zenFreeProfileState.exists
	agentChanged := !bytes.Equal(agentState.data, agentData) || agentState.mode.Perm() != agentMode.Perm() || !agentState.exists
	result := SetupResult{CodexHome: home, ConfigPath: configPath, CatalogPath: catalogPath, ProfilePath: profilePath, ZenFreeProfilePath: zenFreeProfilePath, AgentPath: agentPath, Changed: configChanged || catalogChanged || profileChanged || zenFreeProfileChanged || agentChanged}
	if options.DryRun {
		result.Diff = dryRunDiff(configChanged, catalogChanged, profileChanged, zenFreeProfileChanged, agentChanged)
		return result, nil
	}

	if !result.Changed {
		result.Diff = "no changes"
		return result, nil
	}
	backupPath, err := createBackup(home, []fileState{configState, catalogState, profileState, zenFreeProfileState, agentState}, options.Now())
	if err != nil {
		return SetupResult{}, fmt.Errorf("create Codex backup: %w", err)
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return SetupResult{}, fmt.Errorf("create Codex agents directory: %w", err)
	}
	if err := replaceFiles([]fileState{
		{path: configPath, data: configData, mode: configMode, exists: true},
		{path: catalogPath, data: catalogData, mode: catalogMode, exists: true},
		{path: profilePath, data: profileData, mode: profileMode, exists: true},
		{path: zenFreeProfilePath, data: zenFreeProfileData, mode: zenFreeProfileMode, exists: true},
		{path: agentPath, data: agentData, mode: agentMode, exists: true},
	}, []fileState{configState, catalogState, profileState, zenFreeProfileState, agentState}); err != nil {
		return SetupResult{}, fmt.Errorf("write Codex setup atomically: %w (backup: %s)", err, backupPath)
	}
	result.BackupPath = backupPath
	result.Diff = dryRunDiff(configChanged, catalogChanged, profileChanged, zenFreeProfileChanged, agentChanged)
	return result, nil
}

func setupHome(options SetupOptions) (string, error) {
	if options.CodexHome != "" {
		if !filepath.IsAbs(options.CodexHome) {
			return "", fmt.Errorf("Codex home must be an absolute path")
		}
		return filepath.Clean(options.CodexHome), nil
	}
	return ResolveCodexHome(options.Environment)
}

func validateGatewayURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("gateway URL must be an absolute URL without credentials, query, or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("gateway URL must use http or https")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("an http gateway URL must target loopback")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func readFileState(path string) (fileState, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileState{path: path}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	if !info.Mode().IsRegular() {
		return fileState{}, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileState{}, err
	}
	return fileState{path: path, data: data, mode: info.Mode(), exists: true}, nil
}

func secureFileMode(state fileState) os.FileMode {
	if state.exists && state.mode.Perm() != 0 && state.mode.Perm()&0o077 == 0 {
		return state.mode.Perm()
	}
	return managedFileMode
}

// renderConfig manages only the managed provider tables in the user's default
// config.toml and strips the root gateway routing written by the previous
// setup revision, so the default Codex session keeps its built-in models.
func renderConfig(document tomlDocument, home, gatewayURL, zenGatewayURL string) ([]byte, error) {
	source, err := stripStaleGatewayRouting(document, home)
	if err != nil {
		return nil, err
	}
	document, err = parseTOML(source)
	if err != nil {
		return nil, fmt.Errorf("strip stale gateway routing: %w", err)
	}
	source, err = stripLegacyProvider(document)
	if err != nil {
		return nil, err
	}
	document, err = parseTOML(source)
	if err != nil {
		return nil, fmt.Errorf("strip legacy provider table: %w", err)
	}
	goProvider := make([]tomlSetting, len(goProviderSettings))
	copy(goProvider, goProviderSettings)
	for index := range goProvider {
		if goProvider[index].Key == "base_url" {
			goProvider[index].Value = strconv.Quote(gatewayURL)
		}
	}
	data, err := editProvider(document, GoProviderID, goProvider)
	if err != nil {
		return nil, err
	}
	document, err = parseTOML(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse generated Go provider: %w", err)
	}
	zenProvider := make([]tomlSetting, len(zenProviderSettings))
	copy(zenProvider, zenProviderSettings)
	for index := range zenProvider {
		if zenProvider[index].Key == "base_url" {
			zenProvider[index].Value = strconv.Quote(zenGatewayURL)
		}
	}
	data, err = editProvider(document, ZenProviderID, zenProvider)
	if err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	return data, nil
}

// stripStaleGatewayRouting removes the root settings that the first setup
// revision wrote when it forced the default Codex session through the gateway.
// A config is only stripped when it is clearly gateway-routed (model and
// model_provider match a managed or legacy value); user-customized defaults
// are kept.
func stripStaleGatewayRouting(document tomlDocument, home string) (string, error) {
	model, err := documentString(document, "", "model")
	if err != nil || model != GoModelID {
		return strings.Join(document.lines, ""), nil
	}
	provider, err := documentString(document, "", "model_provider")
	if err != nil || (provider != GoProviderID && provider != LegacyProviderID) {
		return strings.Join(document.lines, ""), nil
	}
	remove := map[string]bool{"model": true, "model_provider": true}
	catalog, err := documentString(document, "", "model_catalog_json")
	if err == nil && filepath.Clean(catalog) == filepath.Clean(filepath.Join(home, CatalogFileName)) {
		remove["model_catalog_json"] = true
	}
	for _, setting := range previousReasoningSettings {
		if raw, ok := document.value("", setting.Key); ok && raw == setting.Value {
			remove[setting.Key] = true
		}
	}
	edits := make([]tomlAssignment, 0, len(remove))
	for _, assignment := range document.keys {
		if assignment.table == "" && remove[assignment.key] {
			edits = append(edits, assignment)
		}
	}
	if len(edits) == 0 {
		return strings.Join(document.lines, ""), nil
	}
	sort.Slice(edits, func(left, right int) bool {
		return edits[left].line > edits[right].line
	})
	lines := append([]string(nil), document.lines...)
	for _, edit := range edits {
		lines = removeTOMLAssignmentLines(lines, edit)
	}
	return strings.Join(lines, ""), nil
}

func removeTOMLAssignmentLines(lines []string, assignment tomlAssignment) []string {
	endLine := assignment.endLine
	if endLine < assignment.line || endLine >= len(lines) {
		endLine = assignment.line
	}
	result := make([]string, 0, len(lines)-(endLine-assignment.line))
	result = append(result, lines[:assignment.line]...)
	result = append(result, lines[endLine+1:]...)
	return result
}

// stripLegacyProvider removes the superseded single gateway provider table
// that the v0.1.x setup revision wrote as `[model_providers.opencode-gateway]`.
// The managed Go and Zen tables replace it; keeping the orphaned table would
// retain a stale credential and a dead routing path.
func stripLegacyProvider(document tomlDocument) (string, error) {
	header := -1
	for index, raw := range document.lines {
		withoutComment, _ := tomlCommentFree(strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r"))
		if strings.TrimSpace(withoutComment) == "[model_providers."+LegacyProviderID+"]" {
			header = index
			break
		}
	}
	if header < 0 {
		return strings.Join(document.lines, ""), nil
	}
	sectionEnd := len(document.lines)
	for index := header + 1; index < len(document.lines); index++ {
		withoutComment, _ := tomlCommentFree(strings.TrimSuffix(strings.TrimSuffix(document.lines[index], "\n"), "\r"))
		if isTOMLTableHeader(withoutComment) {
			sectionEnd = index
			break
		}
	}
	lines := append([]string{}, document.lines[:header]...)
	lines = append(lines, document.lines[sectionEnd:]...)
	return strings.Join(lines, ""), nil
}

// renderProfile renders the Codex profile activated with `codex --profile
// <name>`. Profile files are session overlays, so the default session keeps
// Codex's built-in models and providers.
func renderProfile(home, profileFileName, providerID, modelID string) ([]byte, error) {
	content := strings.Join([]string{
		"# Managed by opencode-gateway. Activate a gateway session with:",
		"#   codex --profile " + strings.TrimSuffix(profileFileName, ".config.toml"),
		"# The default session keeps Codex's built-in models and providers.",
		"",
		"model = " + strconv.Quote(modelID),
		"model_provider = " + strconv.Quote(providerID),
		"model_catalog_json = " + strconv.Quote(filepath.Join(home, CatalogFileName)),
		"",
	}, "\n")
	return []byte(content), nil
}

const subagentName = "deepseek_worker"
const subagentContextWindow = 1048576

// renderSubagent renders the custom Codex agent that routes a bounded worker
// subagent through the Go gateway instance. Spawning it does not change the
// main session's model or provider, so it is available from any Codex session.
func renderSubagent() ([]byte, error) {
	content := strings.Join([]string{
		"# Managed by opencode-gateway. Spawn this worker from any Codex session",
		`# (for example: "spawn a deepseek_worker subagent to ...") to run bounded`,
		"# text tasks on " + GoModelID + " through the local gateway.",
		"",
		"name = " + strconv.Quote(subagentName),
		`description = "Fast text-only ` + GoModelID + ` worker for bounded code, log, search, extraction, and reading tasks routed through opencode-gateway."`,
		"model_provider = " + strconv.Quote(GoProviderID),
		"model = " + strconv.Quote(GoModelID),
		"model_context_window = " + strconv.Itoa(subagentContextWindow),
		`developer_instructions = "You are deepseek_worker, a fast, read-only text worker. Complete the bounded task assigned by the calling Codex agent and report results concisely. Do not use tools that mutate state."`,
		`sandbox_mode = "read-only"`,
		"",
	}, "\n")
	return []byte(content), nil
}

// validateSubagentData verifies the subagent TOML declares a Codex agent
// routed to the gateway.
func validateSubagentData(data []byte) (tomlDocument, error) {
	document, err := parseTOML(string(data))
	if err != nil {
		return tomlDocument{}, err
	}
	for _, key := range []string{"name", "model", "model_provider", "model_context_window", "developer_instructions"} {
		if _, ok := document.value("", key); !ok {
			return tomlDocument{}, fmt.Errorf("subagent is missing %s", key)
		}
	}
	provider, err := documentString(document, "", "model_provider")
	if err != nil || provider != GoProviderID {
		return tomlDocument{}, fmt.Errorf("subagent is not routed to %s", GoProviderID)
	}
	return document, nil
}

func editProvider(document tomlDocument, providerID string, settings []tomlSetting) ([]byte, error) {
	lines := document.lines
	tableHeader := -1
	for index, raw := range lines {
		withoutComment, _ := tomlCommentFree(strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r"))
		if strings.TrimSpace(withoutComment) == "[model_providers."+providerID+"]" {
			tableHeader = index
			break
		}
	}
	if tableHeader < 0 {
		result := append([]string{}, lines...)
		if len(result) > 0 && strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(result[len(result)-1], "\n"), "\r")) != "" {
			result = append(result, "\n")
		}
		result = append(result, "[model_providers."+providerID+"]\n")
		for _, setting := range settings {
			result = append(result, setting.Key+" = "+setting.Value+"\n")
		}
		return []byte(strings.Join(result, "")), nil
	}

	sectionEnd := len(lines)
	for index := tableHeader + 1; index < len(lines); index++ {
		withoutComment, _ := tomlCommentFree(strings.TrimSuffix(strings.TrimSuffix(lines[index], "\n"), "\r"))
		if isTOMLTableHeader(withoutComment) {
			sectionEnd = index
			break
		}
	}
	managed := make(map[string]tomlSetting, len(settings))
	for _, setting := range settings {
		managed[setting.Key] = setting
	}
	seen := make(map[string]bool, len(settings))
	result := make([]string, 0, len(lines)+len(settings))
	skipUntil := -1
	for index, raw := range lines {
		if index <= tableHeader || index >= sectionEnd {
			result = append(result, raw)
			continue
		}
		if index <= skipUntil {
			continue
		}
		assignment, ok := documentAssignmentAt(document, index)
		if !ok || assignment.table != "model_providers."+providerID {
			result = append(result, raw)
			continue
		}
		if forbiddenProviderKeys[assignment.key] {
			skipUntil = assignment.endLine
			continue
		}
		setting, ok := managed[assignment.key]
		if !ok {
			result = append(result, raw)
			continue
		}
		seen[assignment.key] = true
		result = append(result, replaceTOMLAssignment(raw, setting.Value))
		skipUntil = assignment.endLine
	}
	missing := make([]tomlSetting, 0, len(settings))
	for _, setting := range settings {
		if !seen[setting.Key] {
			missing = append(missing, setting)
		}
	}
	if len(missing) > 0 {
		insertion := len(result)
		inTarget := false
		for index, raw := range result {
			withoutComment, _ := tomlCommentFree(strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r"))
			header := strings.TrimSpace(withoutComment)
			if header == "[model_providers."+providerID+"]" {
				inTarget = true
				continue
			}
			if inTarget && isTOMLTableHeader(header) {
				insertion = index
				break
			}
		}
		linesToInsert := make([]string, 0, len(missing)+1)
		for _, setting := range missing {
			linesToInsert = append(linesToInsert, setting.Key+" = "+setting.Value+"\n")
		}
		result = append(result[:insertion], append(linesToInsert, result[insertion:]...)...)
	}
	return []byte(strings.Join(result, "")), nil
}

func documentAssignmentAt(document tomlDocument, line int) (tomlAssignment, bool) {
	for _, assignment := range document.keys {
		if assignment.line == line {
			return assignment, true
		}
	}
	return tomlAssignment{}, false
}

func isTOMLTableHeader(value string) bool {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "[") {
		return false
	}
	_, _, err := parseTableHeader(trimmed)
	return err == nil
}

func replaceTOMLAssignmentLines(lines []string, assignment tomlAssignment, value string) []string {
	if assignment.line < 0 || assignment.line >= len(lines) {
		return lines
	}
	endLine := assignment.endLine
	if endLine < assignment.line || endLine >= len(lines) {
		endLine = assignment.line
	}
	result := make([]string, 0, len(lines)-(endLine-assignment.line))
	result = append(result, lines[:assignment.line]...)
	result = append(result, replaceTOMLAssignment(lines[assignment.line], value))
	result = append(result, lines[endLine+1:]...)
	return result
}

func replaceTOMLAssignment(raw, value string) string {
	ending := ""
	if strings.HasSuffix(raw, "\n") {
		ending = "\n"
		raw = strings.TrimSuffix(raw, "\n")
	}
	if strings.HasSuffix(raw, "\r") {
		ending = "\r" + ending
		raw = strings.TrimSuffix(raw, "\r")
	}
	withoutComment, _ := tomlCommentFree(raw)
	equals := tomlEqualsIndex(withoutComment)
	if equals < 0 {
		return raw + ending
	}
	commentAt := len(withoutComment)
	if commentAt < len(raw) {
		// tomlCommentFree returns the prefix before the first comment.
		commentAt = len(withoutComment)
	}
	comment := raw[commentAt:]
	left := raw[:commentAt]
	trimmedLeft := strings.TrimRight(left, " \t")
	spacing := left[len(trimmedLeft):]
	return trimmedLeft[:equals+1] + " " + value + spacing + comment + ending
}

// ConfigValues contains only non-secret gateway selection settings read from
// the generated Codex profile.
type ConfigValues struct {
	Model                         string
	ModelProvider                 string
	ModelCatalogJSON              string
	ModelReasoningEffort          string
	ModelSupportsReasoningSummary bool
	ModelReasoningSummary         string
}

// InspectProfile validates a Codex session layer (the gateway profile) and
// returns its root model selection. Reasoning settings are optional because
// the generated catalog already declares Codex-compatible defaults.
func InspectProfile(data []byte) (ConfigValues, error) {
	document, err := parseTOML(string(data))
	if err != nil {
		return ConfigValues{}, err
	}
	return inspectProfileDocument(document)
}

func inspectProfileDocument(document tomlDocument) (ConfigValues, error) {
	values := ConfigValues{}
	var err error
	if values.Model, err = documentString(document, "", "model"); err != nil {
		return ConfigValues{}, err
	}
	if values.ModelProvider, err = documentString(document, "", "model_provider"); err != nil {
		return ConfigValues{}, err
	}
	if values.ModelCatalogJSON, err = documentString(document, "", "model_catalog_json"); err != nil {
		return ConfigValues{}, err
	}
	if values.ModelReasoningEffort, err = optionalString(document, "", "model_reasoning_effort"); err != nil {
		return ConfigValues{}, err
	}
	if values.ModelSupportsReasoningSummary, err = optionalBool(document, "", "model_supports_reasoning_summaries"); err != nil {
		return ConfigValues{}, err
	}
	if values.ModelReasoningSummary, err = optionalString(document, "", "model_reasoning_summary"); err != nil {
		return ConfigValues{}, err
	}
	return values, nil
}

// ProviderValues contains only non-secret gateway provider settings read from
// the user's Codex config.toml.
type ProviderValues struct {
	Name               string
	BaseURL            string
	WireAPI            string
	SupportsWebsockets bool
	RequestMaxRetries  int
	StreamMaxRetries   int
}

// InspectProvider validates the managed Go provider table and returns its
// non-secret settings.
func InspectProvider(data []byte) (ProviderValues, error) {
	return InspectProviderFor(data, GoProviderID)
}

// InspectProviderFor validates a named managed provider table and returns its
// non-secret settings.
func InspectProviderFor(data []byte, providerID string) (ProviderValues, error) {
	document, err := parseTOML(string(data))
	if err != nil {
		return ProviderValues{}, err
	}
	return inspectProviderDocument(document, providerID)
}

func inspectProviderDocument(document tomlDocument, providerID string) (ProviderValues, error) {
	values := ProviderValues{}
	var err error
	table := "model_providers." + providerID
	if values.Name, err = documentString(document, table, "name"); err != nil {
		return ProviderValues{}, err
	}
	if values.BaseURL, err = documentString(document, table, "base_url"); err != nil {
		return ProviderValues{}, err
	}
	if values.WireAPI, err = documentString(document, table, "wire_api"); err != nil {
		return ProviderValues{}, err
	}
	if values.SupportsWebsockets, err = documentBool(document, table, "supports_websockets"); err != nil {
		return ProviderValues{}, err
	}
	if values.RequestMaxRetries, err = documentInt(document, table, "request_max_retries"); err != nil {
		return ProviderValues{}, err
	}
	if values.StreamMaxRetries, err = documentInt(document, table, "stream_max_retries"); err != nil {
		return ProviderValues{}, err
	}
	return values, nil
}

func optionalString(document tomlDocument, table, key string) (string, error) {
	raw, ok := document.value(table, key)
	if !ok {
		return "", nil
	}
	return parseTOMLString(raw)
}

func optionalBool(document tomlDocument, table, key string) (bool, error) {
	raw, ok := document.value(table, key)
	if !ok {
		return false, nil
	}
	if raw != "true" && raw != "false" {
		return false, fmt.Errorf("setting %s is not a boolean", key)
	}
	return raw == "true", nil
}

func documentString(document tomlDocument, table, key string) (string, error) {
	value, ok := document.value(table, key)
	if !ok {
		return "", fmt.Errorf("missing Codex setting %s", key)
	}
	decoded, err := parseTOMLString(value)
	if err != nil {
		return "", fmt.Errorf("setting %s is not a string", key)
	}
	return decoded, nil
}

func documentBool(document tomlDocument, table, key string) (bool, error) {
	value, ok := document.value(table, key)
	if !ok || (value != "true" && value != "false") {
		return false, fmt.Errorf("setting %s is not a boolean", key)
	}
	return value == "true", nil
}

func documentInt(document tomlDocument, table, key string) (int, error) {
	value, ok := document.value(table, key)
	if !ok {
		return 0, fmt.Errorf("missing Codex setting %s", key)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("setting %s is not an integer", key)
	}
	return parsed, nil
}

func dryRunDiff(configChanged, catalogChanged, goProfileChanged, zenFreeProfileChanged, agentChanged bool) string {
	if !configChanged && !catalogChanged && !goProfileChanged && !zenFreeProfileChanged && !agentChanged {
		return "no changes"
	}
	var lines []string
	if configChanged {
		lines = append(lines,
			"--- config.toml",
			"+++ config.toml",
			"@@ managed gateway providers @@",
			"~ opencode-gateway-go and opencode-gateway-zen provider urls and retry settings <redacted>",
		)
	}
	if goProfileChanged {
		lines = append(lines,
			"--- "+GoProfileFileName,
			"+++ "+GoProfileFileName,
			"+ gateway Go session profile (model/provider/catalog) <redacted>",
		)
	}
	if zenFreeProfileChanged {
		lines = append(lines,
			"--- "+ZenFreeProfileFileName,
			"+++ "+ZenFreeProfileFileName,
			"+ gateway Zen Free session profile (model/provider/catalog) <redacted>",
		)
	}
	if agentChanged {
		lines = append(lines,
			"--- "+AgentsDirName+"/"+SubagentFileName,
			"+++ "+AgentsDirName+"/"+SubagentFileName,
			"+ gateway subagent definition <redacted>",
		)
	}
	if catalogChanged {
		lines = append(lines,
			"--- models.json",
			"+++ models.json",
			"+ generated deepseek-v4-flash and deepseek-v4-flash-free metadata <redacted>",
		)
	}
	lines = append(lines, "(dry-run: no files or backup written)")
	return strings.Join(lines, "\n")
}

func createBackup(home string, states []fileState, now time.Time) (string, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	backupPath := filepath.Join(home, BackupPrefix+stamp)
	for suffix := 1; ; suffix++ {
		_, statErr := os.Stat(backupPath)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
		backupPath = filepath.Join(home, fmt.Sprintf("%s%s-%d", BackupPrefix, stamp, suffix))
	}
	if err := os.Mkdir(backupPath, 0o700); err != nil {
		return "", err
	}
	manifest := backupManifest{Version: 1, Files: make([]backupFile, 0, len(states))}
	for _, state := range states {
		relative, err := filepath.Rel(home, state.path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("backup file is outside Codex home")
		}
		entry := backupFile{Name: relative, Exists: state.exists, Mode: secureFileMode(state)}
		manifest.Files = append(manifest.Files, entry)
		if !state.exists {
			continue
		}
		target := filepath.Join(backupPath, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, state.data, managedFileMode); err != nil {
			return "", err
		}
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	manifestData = append(manifestData, '\n')
	if err := os.WriteFile(filepath.Join(backupPath, "manifest.json"), manifestData, managedFileMode); err != nil {
		return "", err
	}
	return backupPath, nil
}

func replaceFiles(next, previous []fileState) error {
	return replaceFilesWith(next, previous, os.Rename)
}

func replaceFilesWith(next, previous []fileState, rename func(string, string) error) error {
	temps := make(map[int]string, len(next))
	cleanup := func() {
		for _, name := range temps {
			_ = os.Remove(name)
		}
	}
	for index, state := range next {
		if !state.exists {
			continue
		}
		temp, err := stageFile(state.path, state.data, state.mode)
		if err != nil {
			cleanup()
			return err
		}
		temps[index] = temp
	}
	committed := make([]int, 0, len(next))
	for index, state := range next {
		if !state.exists {
			if err := os.Remove(state.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanup()
				rollbackErr := rollbackFiles(next, previous, committed)
				if rollbackErr != nil {
					return fmt.Errorf("remove %s: %w; rollback failed: %v", filepath.Base(state.path), err, rollbackErr)
				}
				return fmt.Errorf("remove %s: %w", filepath.Base(state.path), err)
			}
			committed = append(committed, index)
			continue
		}
		temp := temps[index]
		if err := rename(temp, state.path); err != nil {
			cleanup()
			rollbackErr := rollbackFiles(next, previous, committed)
			if rollbackErr != nil {
				return fmt.Errorf("replace %s: %w; rollback failed: %v", filepath.Base(state.path), err, rollbackErr)
			}
			return fmt.Errorf("replace %s: %w", filepath.Base(state.path), err)
		}
		committed = append(committed, index)
	}
	return nil
}

func stageFile(path string, data []byte, mode os.FileMode) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".opencode-gateway-*")
	if err != nil {
		return "", err
	}
	temp := file.Name()
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	return temp, nil
}

func rollbackFiles(next, previous []fileState, committed []int) error {
	var rollbackErr error
	for _, index := range committed {
		if previous[index].exists {
			if err := writeAtomic(previous[index].path, previous[index].data, previous[index].mode.Perm()); err != nil && rollbackErr == nil {
				rollbackErr = err
			}
			continue
		}
		if err := os.Remove(next[index].path); err != nil && !errors.Is(err, os.ErrNotExist) && rollbackErr == nil {
			rollbackErr = err
		}
	}
	return rollbackErr
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	temp, err := stageFile(path, data, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// RestoreBackup restores the exact files recorded by a setup backup. The
// backup must be a directory created beneath the selected Codex home; this
// prevents an arbitrary path from becoming a destructive restore target.
func RestoreBackup(environment Environment, codexHome, backupPath string) (SetupResult, error) {
	if codexHome == "" {
		var err error
		codexHome, err = ResolveCodexHome(environment)
		if err != nil {
			return SetupResult{}, err
		}
	}
	if !filepath.IsAbs(codexHome) || !filepath.IsAbs(backupPath) {
		return SetupResult{}, fmt.Errorf("Codex home and backup must be absolute paths")
	}
	backupPath = filepath.Clean(backupPath)
	codexHome = filepath.Clean(codexHome)
	relative, err := filepath.Rel(codexHome, backupPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || !strings.HasPrefix(filepath.Base(backupPath), BackupPrefix) {
		return SetupResult{}, fmt.Errorf("backup is outside the selected Codex home")
	}
	realHome, err := filepath.EvalSymlinks(codexHome)
	if err != nil {
		return SetupResult{}, fmt.Errorf("resolve Codex home: %w", err)
	}
	realBackup, err := filepath.EvalSymlinks(backupPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("resolve backup: %w", err)
	}
	realRelative, err := filepath.Rel(realHome, realBackup)
	if err != nil || realRelative == ".." || strings.HasPrefix(realRelative, ".."+string(filepath.Separator)) || !strings.HasPrefix(filepath.Base(realBackup), BackupPrefix) {
		return SetupResult{}, fmt.Errorf("backup is outside the selected Codex home")
	}
	codexHome, backupPath = realHome, realBackup
	backupInfo, err := os.Stat(backupPath)
	if err != nil || !backupInfo.IsDir() {
		return SetupResult{}, fmt.Errorf("backup is not a directory")
	}
	manifestPath := filepath.Join(backupPath, "manifest.json")
	manifestFileInfo, err := os.Lstat(manifestPath)
	if err != nil || !manifestFileInfo.Mode().IsRegular() {
		return SetupResult{}, fmt.Errorf("backup manifest is not a regular file")
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil || manifest.Version != 1 {
		return SetupResult{}, fmt.Errorf("invalid setup backup manifest")
	}
	previous := make([]fileState, 0, len(manifest.Files))
	next := make([]fileState, 0, len(manifest.Files))
	seenNames := make(map[string]bool, len(manifest.Files))
	for _, entry := range manifest.Files {
		if !isManagedBackupPath(entry.Name) {
			return SetupResult{}, fmt.Errorf("backup contains an unsupported file")
		}
		if seenNames[entry.Name] {
			return SetupResult{}, fmt.Errorf("backup contains a duplicate file")
		}
		seenNames[entry.Name] = true
		target, err := resolveTargetWithinHome(codexHome, entry.Name)
		if err != nil {
			return SetupResult{}, fmt.Errorf("backup file escapes Codex home")
		}
		old, err := readFileState(target)
		if err != nil {
			return SetupResult{}, err
		}
		previous = append(previous, old)
		if !entry.Exists {
			next = append(next, fileState{path: target, exists: false})
			continue
		}
		backupFilePath := filepath.Join(backupPath, entry.Name)
		backupFileInfo, err := os.Lstat(backupFilePath)
		if err != nil || !backupFileInfo.Mode().IsRegular() {
			return SetupResult{}, fmt.Errorf("backup file is not a regular file")
		}
		data, err := os.ReadFile(backupFilePath)
		if err != nil {
			return SetupResult{}, fmt.Errorf("read backup file: %w", err)
		}
		switch entry.Name {
		case ConfigFileName:
			if _, err := parseTOML(string(data)); err != nil {
				return SetupResult{}, fmt.Errorf("backup config is invalid: %w", err)
			}
		case CatalogFileName:
			if _, err := ValidateCatalog(data); err != nil {
				return SetupResult{}, fmt.Errorf("backup catalog is invalid: %w", err)
			}
		case GoProfileFileName:
			if _, err := InspectProfile(data); err != nil {
				return SetupResult{}, fmt.Errorf("backup profile is invalid: %w", err)
			}
		case ZenFreeProfileFileName:
			if _, err := InspectProfile(data); err != nil {
				return SetupResult{}, fmt.Errorf("backup Zen Free profile is invalid: %w", err)
			}
		case filepath.Join(AgentsDirName, SubagentFileName):
			if _, err := validateSubagentData(data); err != nil {
				return SetupResult{}, fmt.Errorf("backup subagent is invalid: %w", err)
			}
		}
		next = append(next, fileState{path: target, data: data, mode: secureBackupMode(entry.Mode), exists: true})
	}
	if err := replaceFiles(next, previous); err != nil {
		return SetupResult{}, fmt.Errorf("restore backup atomically: %w", err)
	}
	return SetupResult{CodexHome: codexHome, ConfigPath: filepath.Join(codexHome, ConfigFileName), CatalogPath: filepath.Join(codexHome, CatalogFileName), ProfilePath: filepath.Join(codexHome, GoProfileFileName), ZenFreeProfilePath: filepath.Join(codexHome, ZenFreeProfileFileName), AgentPath: filepath.Join(codexHome, AgentsDirName, SubagentFileName), BackupPath: backupPath, Changed: true, Diff: "restored setup backup"}, nil
}

// isManagedBackupPath reports whether a manifest entry names one of the files
// managed by setup.
func isManagedBackupPath(name string) bool {
	switch filepath.Clean(name) {
	case ConfigFileName, CatalogFileName, GoProfileFileName, ZenFreeProfileFileName, filepath.Join(AgentsDirName, SubagentFileName):
		return true
	default:
		return false
	}
}

// resolveTargetWithinHome resolves a backup-relative path against Codex home
// without allowing traversal outside it. Symlinks are resolved afterwards by
// the caller only for the backup location, so this stays lexical until the
// final home check that restore already performs.
func resolveTargetWithinHome(codexHome, value string) (string, error) {
	cleanHome := filepath.Clean(codexHome)
	cleanValue := filepath.Clean(value)
	if cleanValue == "" || strings.HasPrefix(cleanValue, string(filepath.Separator)) {
		return "", fmt.Errorf("backup path is not relative")
	}
	if cleanValue == ".." || strings.HasPrefix(cleanValue, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("backup path escapes Codex home")
	}
	resolved := filepath.Join(cleanHome, cleanValue)
	relative, err := filepath.Rel(cleanHome, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("backup path escapes Codex home")
	}
	return resolved, nil
}

func secureBackupMode(mode os.FileMode) os.FileMode {
	if mode.Perm() != 0 && mode.Perm()&0o077 == 0 {
		return mode.Perm()
	}
	return managedFileMode
}
