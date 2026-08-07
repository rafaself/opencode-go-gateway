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
	Environment Environment
	CodexHome   string
	GatewayURL  string
	DryRun      bool
	Now         func() time.Time
}

type SetupResult struct {
	CodexHome   string
	ConfigPath  string
	CatalogPath string
	BackupPath  string
	Changed     bool
	Diff        string
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

var rootSettings = []tomlSetting{
	{Key: "model", Value: `"` + ModelID + `"`},
	{Key: "model_provider", Value: `"` + ProviderID + `"`},
	{Key: "model_catalog_json", Value: ""},
	{Key: "model_reasoning_effort", Value: `"high"`},
	{Key: "model_supports_reasoning_summaries", Value: "false"},
	{Key: "model_reasoning_summary", Value: `"none"`},
}

var providerSettings = []tomlSetting{
	{Key: "name", Value: `"OpenCode Gateway"`},
	{Key: "base_url", Value: ""},
	{Key: "wire_api", Value: `"responses"`},
	{Key: "supports_websockets", Value: "false"},
	{Key: "request_max_retries", Value: "0"},
	{Key: "stream_max_retries", Value: "0"},
}

var forbiddenProviderKeys = map[string]bool{
	"experimental_bearer_token": true,
	"env_key":                   true,
	"env_key_instructions":      true,
}

func (options SetupOptions) withDefaults() SetupOptions {
	if options.GatewayURL == "" {
		options.GatewayURL = DefaultGatewayURL
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return options
}

// SetupCodex creates or updates only the user-level Codex config and catalog.
// Existing source lines are retained except for managed keys, and all target
// files are staged and validated before any target replacement occurs.
func SetupCodex(options SetupOptions) (SetupResult, error) {
	options = options.withDefaults()
	home, err := setupHome(options)
	if err != nil {
		return SetupResult{}, err
	}
	if err := validateGatewayURL(options.GatewayURL); err != nil {
		return SetupResult{}, err
	}
	configPath := filepath.Join(home, ConfigFileName)
	catalogPath := filepath.Join(home, CatalogFileName)
	configState, err := readFileState(configPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read Codex config: %w", err)
	}
	catalogState, err := readFileState(catalogPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read model catalog: %w", err)
	}

	configSource := string(configState.data)
	configDocument, err := parseTOML(configSource)
	if err != nil {
		return SetupResult{}, fmt.Errorf("validate existing Codex config: %w", err)
	}
	catalogData, err := GenerateCatalog()
	if err != nil {
		return SetupResult{}, err
	}
	if _, err := ValidateCatalog(catalogData); err != nil {
		return SetupResult{}, fmt.Errorf("validate generated model catalog: %w", err)
	}
	configData, err := renderConfig(configDocument, home, options.GatewayURL)
	if err != nil {
		return SetupResult{}, err
	}
	if _, err := inspectConfigData(configData); err != nil {
		return SetupResult{}, fmt.Errorf("validate generated Codex config: %w", err)
	}

	configMode := secureFileMode(configState)
	catalogMode := secureFileMode(catalogState)
	configChanged := !bytes.Equal(configState.data, configData) || configState.mode.Perm() != configMode.Perm() || !configState.exists
	catalogChanged := !bytes.Equal(catalogState.data, catalogData) || catalogState.mode.Perm() != catalogMode.Perm() || !catalogState.exists
	result := SetupResult{CodexHome: home, ConfigPath: configPath, CatalogPath: catalogPath, Changed: configChanged || catalogChanged}
	if options.DryRun {
		result.Diff = dryRunDiff(configChanged, catalogChanged)
		return result, nil
	}

	if !result.Changed {
		result.Diff = "no changes"
		return result, nil
	}
	backupPath, err := createBackup(home, []fileState{configState, catalogState}, options.Now())
	if err != nil {
		return SetupResult{}, fmt.Errorf("create Codex backup: %w", err)
	}
	if err := replaceFiles([]fileState{
		{path: configPath, data: configData, mode: configMode, exists: true},
		{path: catalogPath, data: catalogData, mode: catalogMode, exists: true},
	}, []fileState{configState, catalogState}); err != nil {
		return SetupResult{}, fmt.Errorf("write Codex setup atomically: %w (backup: %s)", err, backupPath)
	}
	result.BackupPath = backupPath
	result.Diff = dryRunDiff(configChanged, catalogChanged)
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

func renderConfig(document tomlDocument, home, gatewayURL string) ([]byte, error) {
	settings := make([]tomlSetting, len(rootSettings))
	copy(settings, rootSettings)
	for index := range settings {
		if settings[index].Key == "model_catalog_json" {
			settings[index].Value = strconv.Quote(filepath.Join(home, CatalogFileName))
		}
	}
	data, err := editRoot(document, settings)
	if err != nil {
		return nil, err
	}
	document, err = parseTOML(string(data))
	if err != nil {
		return nil, fmt.Errorf("edit root settings: %w", err)
	}
	provider := make([]tomlSetting, len(providerSettings))
	copy(provider, providerSettings)
	for index := range provider {
		if provider[index].Key == "base_url" {
			provider[index].Value = strconv.Quote(gatewayURL)
		}
	}
	data, err = editProvider(document, provider)
	if err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	return data, nil
}

func editRoot(document tomlDocument, settings []tomlSetting) ([]byte, error) {
	lines := append([]string(nil), document.lines...)
	missing := make([]tomlSetting, 0, len(settings))
	type assignmentEdit struct {
		assignment tomlAssignment
		value      string
	}
	edits := make([]assignmentEdit, 0, len(settings))
	for _, setting := range settings {
		assignment, ok := document.keys[setting.Key]
		if !ok {
			missing = append(missing, setting)
			continue
		}
		edits = append(edits, assignmentEdit{assignment: assignment, value: setting.Value})
	}
	sort.Slice(edits, func(left, right int) bool {
		return edits[left].assignment.line > edits[right].assignment.line
	})
	for _, edit := range edits {
		lines = replaceTOMLAssignmentLines(lines, edit.assignment, edit.value)
	}
	if len(missing) > 0 {
		firstTable := len(lines)
		for index, raw := range lines {
			withoutComment, _ := tomlCommentFree(strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r"))
			if isTOMLTableHeader(withoutComment) {
				firstTable = index
				break
			}
		}
		insert := []string{}
		if firstTable > 0 && strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(lines[firstTable-1], "\n"), "\r")) != "" {
			insert = append(insert, "\n")
		}
		for _, setting := range missing {
			insert = append(insert, setting.Key+" = "+setting.Value+"\n")
		}
		if firstTable < len(lines) {
			insert = append(insert, "\n")
		}
		lines = append(lines[:firstTable], append(insert, lines[firstTable:]...)...)
	}
	return []byte(strings.Join(lines, "")), nil
}

func editProvider(document tomlDocument, settings []tomlSetting) ([]byte, error) {
	lines := document.lines
	tableHeader := -1
	for index, raw := range lines {
		withoutComment, _ := tomlCommentFree(strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r"))
		if strings.TrimSpace(withoutComment) == "[model_providers."+ProviderID+"]" {
			tableHeader = index
			break
		}
	}
	if tableHeader < 0 {
		result := append([]string{}, lines...)
		if len(result) > 0 && strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(result[len(result)-1], "\n"), "\r")) != "" {
			result = append(result, "\n")
		}
		result = append(result, "[model_providers."+ProviderID+"]\n")
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
		if !ok || assignment.table != "model_providers."+ProviderID {
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
			if header == "[model_providers."+ProviderID+"]" {
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

func inspectConfigData(data []byte) (ConfigValues, error) {
	document, err := parseTOML(string(data))
	if err != nil {
		return ConfigValues{}, err
	}
	return inspectDocument(document)
}

// ConfigValues contains only non-secret Codex settings used by setup/doctor.
type ConfigValues struct {
	Model                         string
	ModelProvider                 string
	ModelCatalogJSON              string
	ModelReasoningEffort          string
	ModelSupportsReasoningSummary bool
	ModelReasoningSummary         string
	ProviderName                  string
	ProviderBaseURL               string
	ProviderWireAPI               string
	ProviderSupportsWebsockets    bool
	RequestMaxRetries             int
	StreamMaxRetries              int
}

func InspectConfig(data []byte) (ConfigValues, error) {
	return inspectConfigData(data)
}

func inspectDocument(document tomlDocument) (ConfigValues, error) {
	values := ConfigValues{}
	var err error
	values.Model, err = documentString(document, "", "model")
	if err != nil {
		return ConfigValues{}, err
	}
	values.ModelProvider, err = documentString(document, "", "model_provider")
	if err != nil {
		return ConfigValues{}, err
	}
	values.ModelCatalogJSON, err = documentString(document, "", "model_catalog_json")
	if err != nil {
		return ConfigValues{}, err
	}
	values.ModelReasoningEffort, err = documentString(document, "", "model_reasoning_effort")
	if err != nil {
		return ConfigValues{}, err
	}
	values.ModelSupportsReasoningSummary, err = documentBool(document, "", "model_supports_reasoning_summaries")
	if err != nil {
		return ConfigValues{}, err
	}
	values.ModelReasoningSummary, err = documentString(document, "", "model_reasoning_summary")
	if err != nil {
		return ConfigValues{}, err
	}
	providerTable := "model_providers." + ProviderID
	values.ProviderName, err = documentString(document, providerTable, "name")
	if err != nil {
		return ConfigValues{}, err
	}
	values.ProviderBaseURL, err = documentString(document, providerTable, "base_url")
	if err != nil {
		return ConfigValues{}, err
	}
	values.ProviderWireAPI, err = documentString(document, providerTable, "wire_api")
	if err != nil {
		return ConfigValues{}, err
	}
	values.ProviderSupportsWebsockets, err = documentBool(document, providerTable, "supports_websockets")
	if err != nil {
		return ConfigValues{}, err
	}
	values.RequestMaxRetries, err = documentInt(document, providerTable, "request_max_retries")
	if err != nil {
		return ConfigValues{}, err
	}
	values.StreamMaxRetries, err = documentInt(document, providerTable, "stream_max_retries")
	if err != nil {
		return ConfigValues{}, err
	}
	return values, nil
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

func dryRunDiff(configChanged, catalogChanged bool) string {
	if !configChanged && !catalogChanged {
		return "no changes"
	}
	var lines []string
	if configChanged {
		lines = append(lines,
			"--- config.toml",
			"+++ config.toml",
			"@@ managed Codex settings @@",
			"~ model, provider, catalog, reasoning, and retry values <redacted>",
		)
	}
	if catalogChanged {
		lines = append(lines,
			"--- models.json",
			"+++ models.json",
			"+ generated deepseek-v4-flash metadata <redacted>",
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
		name := filepath.Base(state.path)
		entry := backupFile{Name: name, Exists: state.exists, Mode: secureFileMode(state)}
		manifest.Files = append(manifest.Files, entry)
		if !state.exists {
			continue
		}
		if err := os.WriteFile(filepath.Join(backupPath, name), state.data, managedFileMode); err != nil {
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
		if entry.Name != ConfigFileName && entry.Name != CatalogFileName {
			return SetupResult{}, fmt.Errorf("backup contains an unsupported file")
		}
		if seenNames[entry.Name] {
			return SetupResult{}, fmt.Errorf("backup contains a duplicate file")
		}
		seenNames[entry.Name] = true
		target := filepath.Join(filepath.Clean(codexHome), entry.Name)
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
		if entry.Name == ConfigFileName {
			if _, err := parseTOML(string(data)); err != nil {
				return SetupResult{}, fmt.Errorf("backup config is invalid: %w", err)
			}
		} else if _, err := ValidateCatalog(data); err != nil {
			return SetupResult{}, fmt.Errorf("backup catalog is invalid: %w", err)
		}
		next = append(next, fileState{path: target, data: data, mode: secureBackupMode(entry.Mode), exists: true})
	}
	if err := replaceFiles(next, previous); err != nil {
		return SetupResult{}, fmt.Errorf("restore backup atomically: %w", err)
	}
	return SetupResult{CodexHome: codexHome, ConfigPath: filepath.Join(codexHome, ConfigFileName), CatalogPath: filepath.Join(codexHome, CatalogFileName), BackupPath: backupPath, Changed: true, Diff: "restored setup backup"}, nil
}

func secureBackupMode(mode os.FileMode) os.FileMode {
	if mode.Perm() != 0 && mode.Perm()&0o077 == 0 {
		return mode.Perm()
	}
	return managedFileMode
}
