package codex

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGeneratedCodexV01ProfileDisablesRequestAndStreamRetries(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller did not return the test source path")
	}
	profilePath := filepath.Join(filepath.Dir(sourceFile), "..", "..", "profiles", "codex-v0.1.toml")
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	profile := string(data)
	for _, line := range []string{
		`model = "deepseek-v4-flash"`,
		`model_provider = "opencode-gateway"`,
		`model_supports_reasoning_summaries = false`,
		`supports_websockets = false`,
		`wire_api = "responses"`,
		`request_max_retries = 0`,
		`stream_max_retries = 0`,
	} {
		if !strings.Contains(profile, line) {
			t.Fatalf("profile does not contain %q: %s", line, profile)
		}
	}
}
