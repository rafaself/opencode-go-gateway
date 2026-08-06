package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
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
