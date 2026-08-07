package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/credentials"
)

func TestVersionPrintsBuildMetadata(t *testing.T) {
	previousVersion, previousCommit, previousBuildDate := version, commit, buildDate
	version, commit, buildDate = "v1.2.3", "abc1234", "2026-08-05T12:34:56Z"
	t.Cleanup(func() {
		version, commit, buildDate = previousVersion, previousCommit, previousBuildDate
	})

	var stdout, stderr bytes.Buffer
	if err := execute([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"opencode-gateway version=v1.2.3 commit=abc1234 build_date=2026-08-05T12:34:56Z",
		"go=",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("version output does not contain %q: %s", expected, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "test-api-key") {
		t.Fatalf("version output exposed a secret: %s", stdout.String())
	}
}

func TestHelpAndVersionAliasesAreSuccessfulAndSafe(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"-v"}, {"--version"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := commandExitCode(args, &stdout, &stderr); got != 0 {
				t.Fatalf("commandExitCode(%v) = %d; stdout=%s stderr=%s", args, got, stdout.String(), stderr.String())
			}
			if strings.ContainsAny(stdout.String(), "\r\n") && args[0] != "help" && args[0] != "-h" && args[0] != "--help" {
				// Version output is expected to contain one newline, but no metadata
				// value may inject an additional line.
				if strings.Count(stdout.String(), "\n") != 1 {
					t.Fatalf("version output contains injected lines: %q", stdout.String())
				}
			}
			if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
				if !strings.Contains(stdout.String(), "Exit status: 0 success/help") {
					t.Fatalf("help output = %s", stdout.String())
				}
			} else if !strings.Contains(stdout.String(), "opencode-gateway version=") {
				t.Fatalf("version output = %s", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("successful command wrote stderr: %s", stderr.String())
			}
		})
	}
}

func TestVersionSanitizesInjectedBuildMetadata(t *testing.T) {
	previousVersion, previousCommit, previousBuildDate := version, commit, buildDate
	version, commit, buildDate = "v0.1.0\nsecret", "commit\rvalue", "date\tvalue"
	t.Cleanup(func() {
		version, commit, buildDate = previousVersion, previousCommit, previousBuildDate
	})

	var stdout bytes.Buffer
	if err := execute([]string{"version"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "\nsecret") || strings.Contains(stdout.String(), "\r") || strings.Contains(stdout.String(), "\t") || strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("version output was not safely normalized: %q", stdout.String())
	}
}

func TestInvalidRunArgumentsUseUsageExitCode(t *testing.T) {
	for _, args := range [][]string{
		{"run", "-unknown"},
		{"run", "unexpected"},
		{"dev", "capture-codex", "-unknown"},
		{"dev", "capture-codex", "unexpected"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := commandExitCode(args, &stdout, &stderr); got != 2 {
				t.Fatalf("commandExitCode(%v) = %d, want 2; stderr=%s", args, got, stderr.String())
			}
		})
	}
}

func TestOperationalFailureUsesExitCodeOne(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	var stdout, stderr bytes.Buffer
	if got := commandExitCode([]string{"run"}, &stdout, &stderr); got != 1 {
		t.Fatalf("commandExitCode(run) = %d, want 1; stdout=%s stderr=%s", got, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "OPENCODE_GO_API_KEY is required") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestSetupCodexCommandWritesUserConfigWithoutGatewayKey(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := execute([]string{"setup", "codex", "--codex-home", home}, &stdout, &stderr); err != nil {
		t.Fatalf("setup codex error = %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Backup:") || !strings.Contains(stdout.String(), "--profile opencode-gateway") || strings.Contains(stdout.String(), "OPENCODE_GO_API_KEY") {
		t.Fatalf("setup output = %s", stdout.String())
	}
	for _, name := range []string{"config.toml", "models.json", "opencode-gateway-go.config.toml", "opencode-gateway-zen-free.config.toml", filepath.Join("agents", "deepseek-worker.toml")} {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Fatalf("setup did not create %s: %v", name, err)
		}
	}
}

func TestSetupCodexDryRunCommandDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := execute([]string{"setup", "codex", "--codex-home", home, "--dry-run"}, &stdout, &stderr); err != nil {
		t.Fatalf("dry-run error = %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "redacted") {
		t.Fatalf("dry-run output = %s", stdout.String())
	}
	if entries, err := os.ReadDir(home); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("dry-run wrote entries: %+v", entries)
	}
}

func TestConfigSetKeyStoresWithoutEchoingTheCredential(t *testing.T) {
	store := credentials.NewFileStore(filepath.Join(t.TempDir(), "opencode-gateway", "credentials"))
	const key = "sk-main-test-secret"
	lookup := func(string) (string, bool) { return "", false }
	var stdout, stderr bytes.Buffer
	if err := runConfigWithStore([]string{"set-key", "--stdin"}, &stdout, &stderr, store, lookup, strings.NewReader(key+"\n")); err != nil {
		t.Fatalf("set-key error = %v; stderr=%s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), key) || strings.Contains(stderr.String(), key) {
		t.Fatalf("set-key output exposed API key: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "permission-restricted") {
		t.Fatalf("set-key output did not describe protected storage: %s", stdout.String())
	}

	settings, err := loadRuntimeConfig(lookup, store)
	if err != nil {
		t.Fatal(err)
	}
	if settings.APIKey() != key {
		t.Fatalf("loaded API key = %q, want stored key", settings.APIKey())
	}

	stdout.Reset()
	if err := runConfigWithStore([]string{"status"}, &stdout, &stderr, store, lookup, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), key) || !strings.Contains(stdout.String(), "Stored API key: configured") {
		t.Fatalf("status output = %q", stdout.String())
	}
	if err := runConfigWithStore([]string{"remove-key"}, &stdout, &stderr, store, lookup, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(); err == nil {
		t.Fatal("stored API key remained after remove-key")
	}
}

func TestStoredCredentialDoesNotOverrideEnvironment(t *testing.T) {
	store := credentials.NewFileStore(filepath.Join(t.TempDir(), "opencode-gateway", "credentials"))
	if _, err := store.Save("stored-key"); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) {
		if name == "OPENCODE_GO_API_KEY" {
			return "environment-key", true
		}
		return "", false
	}
	settings, err := loadRuntimeConfig(lookup, store)
	if err != nil {
		t.Fatal(err)
	}
	if settings.APIKey() != "environment-key" {
		t.Fatalf("API key = %q, want environment value", settings.APIKey())
	}
}

func TestConfigSetKeyRejectsCommandLineCredential(t *testing.T) {
	store := credentials.NewFileStore(filepath.Join(t.TempDir(), "opencode-gateway", "credentials"))
	var stdout, stderr bytes.Buffer
	err := runConfigWithStore([]string{"set-key", "sk-command-line-secret"}, &stdout, &stderr, store, func(string) (string, bool) { return "", false }, strings.NewReader(""))
	if !errors.Is(err, errUsage) {
		t.Fatalf("error = %v, want usage error", err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "sk-command-line-secret") {
		t.Fatalf("usage output exposed command-line credential: %q", stdout.String()+stderr.String())
	}
}

func TestCaptureCommandHelpRemainsAvailable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := execute([]string{"dev", "capture-codex", "-h"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Usage of capture-codex") {
		t.Fatalf("capture help was not preserved: %s", stderr.String())
	}
}

func TestRunReportsMissingAPIKeyWithoutEchoingEnvironment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runGateway(context.Background(), func(string) (string, bool) { return "", false }, &stdout, &stderr)
	if err == nil || err.Error() != "OPENCODE_GO_API_KEY is required" {
		t.Fatalf("error = %v", err)
	}
}

func TestRunCommandShutsDownOnSIGTERM(t *testing.T) {
	if os.Getenv("OPENCODE_GATEWAY_SIGNAL_TEST_CHILD") == "1" {
		ctx, stop := signalContextForTest()
		defer stop()
		if err := runGateway(ctx, os.LookupEnv, os.Stdout, os.Stderr); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		return
	}

	command := exec.Command(os.Args[0], "-test.run=TestRunCommandShutsDownOnSIGTERM", "-test.v")
	command.Env = []string{
		"OPENCODE_GATEWAY_SIGNAL_TEST_CHILD=1",
		"OPENCODE_GO_API_KEY=child-api-key-secret",
		"OPENCODE_GATEWAY_PORT=0",
		"OPENCODE_GATEWAY_SHUTDOWN_TIMEOUT=1s",
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "listening") {
				ready <- scanner.Text()
				return
			}
		}
		ready <- ""
	}()
	select {
	case line := <-ready:
		if line == "" {
			t.Fatalf("child exited before readiness; stderr=%s", stderr.String())
		}
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("child did not become ready")
	}

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- command.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("child did not exit cleanly: %v; stderr=%s", err, stderr.String())
		}
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("child did not shut down after SIGTERM")
	}

	combined := stderr.String()
	if strings.Contains(combined, "child-api-key-secret") {
		t.Fatalf("child logs exposed API key: %s", combined)
	}
}

func signalContextForTest() (context.Context, context.CancelFunc) {
	return signalContext(context.Background())
}
