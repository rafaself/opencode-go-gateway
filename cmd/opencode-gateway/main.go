package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/rafaself/opencode-go-gateway/internal/app"
	"github.com/rafaself/opencode-go-gateway/internal/capture"
	"github.com/rafaself/opencode-go-gateway/internal/codexsetup"
	"github.com/rafaself/opencode-go-gateway/internal/config"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

var errUsage = errors.New("invalid command usage")

func main() {
	os.Exit(commandExitCode(os.Args[1:], os.Stdout, os.Stderr))
}

func commandExitCode(args []string, stdout, stderr io.Writer) int {
	if err := execute(args, stdout, stderr); err != nil {
		if errors.Is(err, errUsage) {
			return 2
		}
		fmt.Fprintf(stderr, "opencode-gateway: %v\n", err)
		return 1
	}
	return 0
}

func execute(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errUsage
	}

	switch args[0] {
	case "run":
		return runServer(args[1:], stdout, stderr)
	case "version":
		if len(args) != 1 {
			usage(stderr)
			return errUsage
		}
		return printVersion(stdout)
	case "setup":
		if len(args) < 2 || args[1] != "codex" {
			usage(stderr)
			return errUsage
		}
		return runCodexSetup(args[2:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "dev":
		if len(args) < 2 || args[1] != "capture-codex" {
			usage(stderr)
			return errUsage
		}
		return runCapture(args[2:], stdout, stderr)
	default:
		usage(stderr)
		return errUsage
	}
}

func runServer(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: opencode-gateway run")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Start the local gateway HTTP server using OPENCODE_* environment settings.")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "run: unexpected arguments: %v\n", flags.Args())
		return errUsage
	}

	ctx, stop := signalContext(context.Background())
	defer stop()
	return runGateway(ctx, os.LookupEnv, stdout, stderr)
}

func runGateway(ctx context.Context, lookup config.LookupEnv, stdout, stderr io.Writer) error {
	settings, err := config.Load(lookup)
	if err != nil {
		return err
	}
	logger := app.NewLogger(stderr, settings.LogLevel)
	return app.RunWithBuildMetadata(ctx, settings, logger, func(address string) {
		fmt.Fprintf(stdout, "opencode-gateway listening on http://%s\n", address)
	}, app.BuildMetadata{Version: version, Commit: commit, BuildDate: buildDate})
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "opencode-gateway version=%s commit=%s build_date=%s go=%s\n", version, commit, buildDate, runtime.Version())
	return err
}

func runCodexSetup(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("setup codex", flag.ContinueOnError)
	flags.SetOutput(stderr)
	codexHome := flags.String("codex-home", "", "Codex home directory; defaults to CODEX_HOME or the user home")
	gatewayURL := flags.String("gateway-url", codexsetup.DefaultGatewayURL, "gateway Responses base URL")
	dryRun := flags.Bool("dry-run", false, "show redacted changes without writing files")
	restore := flags.String("restore", "", "restore a setup backup directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if flags.NArg() != 0 || (*restore != "" && *dryRun) {
		fmt.Fprintln(stderr, "setup codex: use either --restore or --dry-run, and provide no positional arguments")
		return errUsage
	}
	if *restore != "" {
		result, err := codexsetup.RestoreBackup(codexsetup.Environment{}, *codexHome, *restore)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Restored Codex setup from backup %s\n", result.BackupPath)
		return nil
	}
	result, err := codexsetup.SetupCodex(codexsetup.SetupOptions{
		CodexHome:  *codexHome,
		GatewayURL: *gatewayURL,
		DryRun:     *dryRun,
	})
	if err != nil {
		return err
	}
	if *dryRun && result.Diff != "" {
		fmt.Fprintln(stdout, result.Diff)
	}
	if *dryRun {
		return nil
	}
	if !result.Changed {
		fmt.Fprintf(stdout, "Codex setup is already current for %s\n", result.ConfigPath)
		return nil
	}
	fmt.Fprintf(stdout, "Codex setup updated %s and %s\n", result.ConfigPath, result.CatalogPath)
	fmt.Fprintf(stdout, "Backup: %s\n", result.BackupPath)
	fmt.Fprintf(stdout, "Rollback: opencode-gateway setup codex --restore %s\n", result.BackupPath)
	return nil
}

func runDoctor(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	codexHome := flags.String("codex-home", "", "Codex home directory; defaults to CODEX_HOME or the user home")
	gatewayURL := flags.String("gateway-url", "", "gateway Responses base URL; defaults to the configured provider")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "doctor: unexpected arguments: %v\n", flags.Args())
		return errUsage
	}
	report := codexsetup.Diagnose(context.Background(), codexsetup.DoctorOptions{CodexHome: *codexHome, GatewayURL: *gatewayURL})
	for _, check := range report.Checks {
		fmt.Fprintf(stdout, "[%s] %s: %s\n", check.Severity, check.Name, check.Message)
	}
	fmt.Fprintf(stdout, "Doctor summary: %d failure(s), %d warning(s)\n", report.Failures(), report.Warnings())
	if report.Failures() > 0 {
		return &codexsetup.DoctorError{Failures: report.Failures()}
	}
	return nil
}

func runCapture(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("capture-codex", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listenAddr := flags.String("listen", "127.0.0.1:0", "loopback address to listen on")
	outputDir := flags.String("output-dir", "testdata/codex/captures", "directory for redacted request fixtures; empty disables writing")
	prefix := flags.String("name", "capture", "fixture filename prefix")
	codexVersion := flags.String("codex-version", "", "Codex CLI version to record; defaults to the request User-Agent version")
	responseMode := flags.String("response", string(capture.ResponseText), "response mode: text, function, parallel, custom, incomplete, or failed")
	responseText := flags.String("response-text", "capture acknowledged", "text returned by the default response mode")
	maxBodyBytes := flags.Int64("max-body-bytes", capture.DefaultMaxBodyBytes, "maximum request body size")
	once := flags.Bool("once", false, "stop after the first captured request")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "capture-codex: unexpected arguments: %v\n", flags.Args())
		return errUsage
	}

	mode, err := capture.ParseResponseMode(*responseMode)
	if err != nil {
		fmt.Fprintf(stderr, "capture-codex: %v\n", err)
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	server, err := capture.Listen(capture.Config{
		ListenAddr:    *listenAddr,
		OutputDir:     *outputDir,
		FixturePrefix: *prefix,
		CodexVersion:  *codexVersion,
		ResponseMode:  mode,
		ResponseText:  *responseText,
		MaxBodyBytes:  *maxBodyBytes,
		OneShot:       *once,
		OnCapture: func(info capture.CaptureInfo) {
			if info.Path == "" {
				fmt.Fprintf(stdout, "captured request %d (fixture writing disabled)\n", info.Sequence)
				return
			}
			fmt.Fprintf(stdout, "captured request %d -> %s\n", info.Sequence, info.Path)
		},
	})
	if err != nil {
		return err
	}
	defer server.Close()

	fmt.Fprintf(stdout, "Codex capture server listening on %s\n", server.BaseURL())
	fmt.Fprintf(stdout, "Configure Codex with model_providers.capture.base_url = %q and wire_api = %q\n", server.BaseURL(), "responses")

	ctx, stop := signalContext(context.Background())
	defer stop()
	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: opencode-gateway <command>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  run                    Start the local gateway server")
	fmt.Fprintln(w, "  version                Print version and build metadata")
	fmt.Fprintln(w, "  setup codex            Configure the user-level Codex provider safely")
	fmt.Fprintln(w, "  doctor                 Diagnose gateway, Codex, and provider setup")
	fmt.Fprintln(w, "  dev capture-codex      Start the development-only contract capture server")
}
