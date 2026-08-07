// Package codexsetup contains the safe, user-level Codex configuration
// workflow used by the setup and doctor commands.
package codexsetup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// GoProviderID routes Codex to the local gateway, which forwards to
	// OpenCode Go (`zen/go/v1`) with the paid deepseek-v4-flash model.
	GoProviderID = "opencode-gateway-go"
	// ZenProviderID routes Codex to the same local gateway, which forwards to
	// the OpenCode Zen endpoint (`zen/v1`). The Zen backend serves both paid
	// and free models; the free models are selected by profile and model ID.
	ZenProviderID = "opencode-gateway-zen"
	// LegacyProviderID is the provider table ID written by the single-provider
	// setup revision (v0.1.x). Setup removes the superseded table so orphaned
	// credentials and dead routing paths are not retained.
	LegacyProviderID = "opencode-gateway"
	// GoModelID is the tagged paid OpenCode Go model. The `(go)` tag routes
	// the request to the OpenCode Go backend; the label is client metadata.
	GoModelID = "deepseek-v4-flash (go)"
	// ZenFreeModelID is the tagged free OpenCode Zen model. The `(zen)` tag
	// routes the request to the OpenCode Zen backend. Free models are
	// time-limited and their data may be used to improve the model; see the
	// OpenCode Zen docs.
	ZenFreeModelID = "deepseek-v4-flash (zen)"
	// UntaggedGoModelID is the Go model value written by the two-gateway setup
	// revision before routing tags were introduced. Setup and doctor still
	// recognize it so previously managed configurations migrate cleanly.
	UntaggedGoModelID = "deepseek-v4-flash"
	// UntaggedZenFreeModelID is the untagged free model value served by the
	// OpenCode Zen backend, as reported by the provider models endpoint.
	UntaggedZenFreeModelID = "deepseek-v4-flash-free"
	ConfigFileName         = "config.toml"
	CatalogFileName        = "models.json"
	// GoProfileFileName is the Codex profile activated with `codex --profile
	// opencode-gateway-go`. Profile files are session overlays; the default
	// Codex session keeps its built-in models and providers.
	GoProfileFileName = "opencode-gateway-go.config.toml"
	// ZenFreeProfileFileName is the Codex profile activated with `codex
	// --profile opencode-gateway-zen-free`. It selects the free model through
	// the shared Zen backend provider.
	ZenFreeProfileFileName = "opencode-gateway-zen-free.config.toml"
	AgentsDirName          = "agents"
	SubagentFileName       = "deepseek-worker.toml"
	BackupPrefix           = "backup-opencode-gateway-"
	// DefaultGoGatewayURL is the local listener for the single gateway
	// instance that serves both the Go and Zen backends.
	DefaultGoGatewayURL = "http://127.0.0.1:8787/v1"
)

// Environment is the small process boundary needed to resolve Codex's user
// directory. Tests can provide a temporary home without changing process-wide
// environment or the current working directory.
type Environment struct {
	LookupEnv   func(string) (string, bool)
	UserHomeDir func() (string, error)
}

func (e Environment) withDefaults() Environment {
	if e.LookupEnv == nil {
		e.LookupEnv = os.LookupEnv
	}
	if e.UserHomeDir == nil {
		e.UserHomeDir = os.UserHomeDir
	}
	return e
}

// ResolveCodexHome follows Codex's documented CODEX_HOME behavior. A supplied
// path must be absolute: resolving a relative value against the process
// working directory would make a user configuration command unsafe.
func ResolveCodexHome(environment Environment) (string, error) {
	environment = environment.withDefaults()
	if value, ok := environment.LookupEnv("CODEX_HOME"); ok {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("CODEX_HOME is set but empty")
		}
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("CODEX_HOME must be an absolute path")
		}
		return filepath.Clean(value), nil
	}

	home, err := environment.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	if strings.TrimSpace(home) == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("user home is not an absolute path")
	}
	return filepath.Join(filepath.Clean(home), ".codex"), nil
}

func resolvePathWithinHome(home, value string) (string, error) {
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("Codex home must be an absolute path")
	}
	if value == "" {
		return "", fmt.Errorf("path is empty")
	}
	cleanHome := filepath.Clean(home)
	cleanValue := filepath.Clean(value)
	if !filepath.IsAbs(cleanValue) {
		cleanValue = filepath.Join(cleanHome, cleanValue)
	}
	relative, err := filepath.Rel(cleanHome, cleanValue)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path is outside Codex home")
	}
	return cleanValue, nil
}
