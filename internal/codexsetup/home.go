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
	ProviderID        = "opencode-gateway"
	ModelID           = "deepseek-v4-flash"
	ConfigFileName    = "config.toml"
	CatalogFileName   = "models.json"
	BackupPrefix      = "backup-opencode-gateway-"
	DefaultGatewayURL = "http://127.0.0.1:8787/v1"
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
