package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/rafaself/opencode-go-gateway/internal/app"
	"github.com/rafaself/opencode-go-gateway/internal/capture"
	"github.com/rafaself/opencode-go-gateway/internal/codexsetup"
	"github.com/rafaself/opencode-go-gateway/internal/config"
	"github.com/rafaself/opencode-go-gateway/internal/credentials"
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
	case "help", "-h", "--help":
		if len(args) != 1 {
			usage(stderr)
			return errUsage
		}
		usage(stdout)
		return nil
	case "-v", "--version":
		if len(args) != 1 {
			usage(stderr)
			return errUsage
		}
		return printVersion(stdout)
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
	case "config":
		return runConfig(args[1:], stdout, stderr)
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
	return runGatewayWithStore(ctx, os.LookupEnv, credentials.Default(), stdout, stderr)
}

func runGateway(ctx context.Context, lookup config.LookupEnv, stdout, stderr io.Writer) error {
	return runGatewayWithStore(ctx, lookup, credentials.Store{}, stdout, stderr)
}

func runGatewayWithStore(ctx context.Context, lookup config.LookupEnv, store credentials.Store, stdout, stderr io.Writer) error {
	settings, err := loadRuntimeConfig(lookup, store)
	if err != nil {
		return err
	}
	logger := app.NewLogger(stderr, settings.LogLevel)
	return app.RunWithBuildMetadata(ctx, settings, logger, func(address string) {
		fmt.Fprintf(stdout, "opencode-gateway listening on http://%s\n", address)
	}, app.BuildMetadata{Version: version, Commit: commit, BuildDate: buildDate})
}

func loadRuntimeConfig(lookup config.LookupEnv, store credentials.Store) (config.Config, error) {
	runtimeLookup, err := lookupWithStoredCredential(lookup, store)
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(runtimeLookup)
}

func lookupWithStoredCredential(lookup config.LookupEnv, store credentials.Store) (config.LookupEnv, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if value, ok := lookup("OPENCODE_GO_API_KEY"); ok && strings.TrimSpace(value) != "" {
		return lookup, nil
	}
	storedKey, _, err := store.Load()
	if err == nil {
		return func(name string) (string, bool) {
			if name == "OPENCODE_GO_API_KEY" {
				return storedKey, true
			}
			return lookup(name)
		}, nil
	}
	if !errors.Is(err, credentials.ErrNotFound) {
		return nil, err
	}
	return lookup, nil
}

func runConfig(args []string, stdout, stderr io.Writer) error {
	return runConfigWithStore(args, stdout, stderr, credentials.Default(), os.LookupEnv, os.Stdin)
}

func runConfigWithStore(args []string, stdout, stderr io.Writer, store credentials.Store, lookup config.LookupEnv, input io.Reader) error {
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			fmt.Fprintln(stderr, "config status: unexpected positional arguments")
			return errUsage
		}
		return showConfigStatus(stdout, store, lookup)
	}

	switch args[0] {
	case "set-key":
		return setStoredAPIKey(args[1:], stdout, stderr, store, input)
	case "remove-key":
		if len(args) != 1 {
			fmt.Fprintf(stderr, "config %s: unexpected positional arguments\n", args[0])
			return errUsage
		}
		if err := store.Remove(); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Stored API key removed.")
		return nil
	case "help", "-h", "--help":
		if len(args) != 1 {
			printConfigUsage(stderr)
			return errUsage
		}
		printConfigUsage(stdout)
		return nil
	default:
		printConfigUsage(stderr)
		return errUsage
	}
}

func showConfigStatus(stdout io.Writer, store credentials.Store, lookup config.LookupEnv) error {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	environmentConfigured := false
	if value, ok := lookup("OPENCODE_GO_API_KEY"); ok {
		environmentConfigured = strings.TrimSpace(value) != ""
	}
	backend, err := store.Status()
	if err != nil {
		return err
	}
	if environmentConfigured {
		fmt.Fprintln(stdout, "Environment API key: configured (takes precedence over stored credentials).")
	} else {
		fmt.Fprintln(stdout, "Environment API key: not configured.")
	}
	switch backend {
	case credentials.BackendKeyring:
		fmt.Fprintln(stdout, "Stored API key: configured in the system keyring.")
	case credentials.BackendFile:
		fmt.Fprintln(stdout, "Stored API key: configured in a permission-restricted local file (not encrypted at rest).")
	default:
		fmt.Fprintln(stdout, "Stored API key: not configured.")
	}
	fmt.Fprintln(stdout, "API key values are never printed or passed to Codex.")
	return nil
}

func setStoredAPIKey(args []string, stdout, stderr io.Writer, store credentials.Store, input io.Reader) error {
	flags := flag.NewFlagSet("config set-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stdinMode := flags.Bool("stdin", false, "read the API key from standard input")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "config set-key: unexpected positional arguments")
		return errUsage
	}
	if input == nil {
		return errors.New("config set-key: standard input is unavailable")
	}
	key, err := readAPIKey(input, stdout, *stdinMode)
	if err != nil {
		return err
	}
	backend, err := store.Save(key)
	if err != nil {
		return err
	}
	switch backend {
	case credentials.BackendKeyring:
		fmt.Fprintln(stdout, "API key stored in the system keyring.")
	case credentials.BackendFile:
		fmt.Fprintln(stdout, "API key stored in a permission-restricted local file (not encrypted at rest).")
	default:
		return errors.New("API key was not stored")
	}
	return nil
}

func readAPIKey(input io.Reader, output io.Writer, stdinMode bool) (string, error) {
	if file, ok := input.(*os.File); ok && isTerminal(file) {
		if stdinMode {
			return "", errors.New("config set-key --stdin requires piped standard input; use config set-key for a hidden terminal prompt")
		}
		return readTerminalAPIKey(file, output)
	}
	data, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil {
		return "", errors.New("read API key from standard input")
	}
	if len(data) > 4096 {
		return "", errors.New("API key input is too long")
	}
	value := strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("API key input must contain exactly one non-empty line")
	}
	for _, runeValue := range value {
		if runeValue < 0x20 || runeValue == 0x7f {
			return "", errors.New("API key input contains control characters")
		}
	}
	return value, nil
}

func readTerminalAPIKey(terminal *os.File, output io.Writer) (string, error) {
	if _, err := fmt.Fprint(output, "OpenCode Go API key: "); err != nil {
		return "", errors.New("write API key prompt")
	}
	if err := setTerminalEcho(terminal, false); err != nil {
		return "", errors.New("cannot disable terminal echo; use --stdin with a secure shell prompt")
	}
	echoDisabled := true
	defer func() {
		if echoDisabled {
			_ = setTerminalEcho(terminal, true)
		}
	}()
	line, err := bufio.NewReader(terminal).ReadString('\n')
	if restoreErr := setTerminalEcho(terminal, true); restoreErr != nil {
		return "", errors.New("cannot restore terminal echo")
	}
	echoDisabled = false
	_, _ = fmt.Fprintln(output)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("read API key from terminal")
	}
	value := strings.TrimSpace(line)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("API key input must contain exactly one non-empty line")
	}
	for _, runeValue := range value {
		if runeValue < 0x20 || runeValue == 0x7f {
			return "", errors.New("API key input contains control characters")
		}
	}
	return value, nil
}

func setTerminalEcho(terminal *os.File, enabled bool) error {
	argument := "-echo"
	if enabled {
		argument = "echo"
	}
	command := exec.Command("stty", argument)
	command.Stdin = terminal
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

func isTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func printConfigUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: ocgtw config <command>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  status                 Show credential status without printing the key")
	fmt.Fprintln(w, "  set-key [--stdin]      Read and store one API key from standard input")
	fmt.Fprintln(w, "  remove-key             Delete the stored API key")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "The key is never accepted as a command-line argument or written to Codex configuration.")
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "opencode-gateway version=%s commit=%s build_date=%s go=%s\n", safeOutputMetadata(version), safeOutputMetadata(commit), safeOutputMetadata(buildDate), safeOutputMetadata(runtime.Version()))
	return err
}

func safeOutputMetadata(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	var result strings.Builder
	for _, runeValue := range value {
		if (runeValue >= 'a' && runeValue <= 'z') ||
			(runeValue >= 'A' && runeValue <= 'Z') ||
			(runeValue >= '0' && runeValue <= '9') ||
			runeValue == '.' || runeValue == '-' || runeValue == '_' || runeValue == '+' || runeValue == ':' {
			result.WriteRune(runeValue)
			continue
		}
		result.WriteByte('-')
	}
	if result.Len() == 0 {
		return "unknown"
	}
	if result.Len() > 128 {
		return result.String()[:128]
	}
	return result.String()
}

func runCodexSetup(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("setup codex", flag.ContinueOnError)
	flags.SetOutput(stderr)
	codexHome := flags.String("codex-home", "", "Codex home directory; defaults to CODEX_HOME or the user home")
	gatewayURL := flags.String("gateway-url", codexsetup.DefaultGoGatewayURL, "gateway Responses base URL for the opencode-gateway-go profile")
	zenGatewayURL := flags.String("zen-gateway-url", codexsetup.DefaultZenGatewayURL, "gateway Responses base URL for the opencode-gateway-zen provider and the opencode-gateway-zen-free profile")
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
		CodexHome:     *codexHome,
		GatewayURL:    *gatewayURL,
		ZenGatewayURL: *zenGatewayURL,
		DryRun:        *dryRun,
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
	fmt.Fprintf(stdout, "Codex setup updated %s, %s, %s, %s, and %s\n", result.ConfigPath, result.CatalogPath, result.ProfilePath, result.ZenFreeProfilePath, result.AgentPath)
	fmt.Fprintf(stdout, "Backup: %s\n", result.BackupPath)
	fmt.Fprintf(stdout, "Rollback: opencode-gateway setup codex --restore %s\n", result.BackupPath)
	fmt.Fprintln(stdout, "The default codex session keeps its built-in models; run `codex --profile opencode-gateway-go` or `codex --profile opencode-gateway-zen-free` to use the gateway")
	return nil
}

func runDoctor(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	codexHome := flags.String("codex-home", "", "Codex home directory; defaults to CODEX_HOME or the user home")
	gatewayURL := flags.String("gateway-url", "", "gateway Responses base URL; defaults to the configured provider")
	zenGatewayURL := flags.String("zen-gateway-url", "", "Zen gateway Responses base URL to verify against the configured provider")
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
	lookup, err := lookupWithStoredCredential(os.LookupEnv, credentials.Default())
	if err != nil {
		return err
	}
	report := codexsetup.Diagnose(context.Background(), codexsetup.DoctorOptions{
		Environment:   codexsetup.Environment{LookupEnv: lookup},
		CodexHome:     *codexHome,
		GatewayURL:    *gatewayURL,
		ZenGatewayURL: *zenGatewayURL,
	})
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
	fmt.Fprintln(w, "Usage: opencode-gateway <command> [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  run                    Start the local gateway server")
	fmt.Fprintln(w, "  version                Print version and build metadata")
	fmt.Fprintln(w, "  help                   Print this help")
	fmt.Fprintln(w, "  setup codex            Configure the user-level Codex provider safely")
	fmt.Fprintln(w, "  doctor                 Diagnose gateway, Codex, and provider setup")
	fmt.Fprintln(w, "  config                 Show or manage the stored API key")
	fmt.Fprintln(w, "  dev capture-codex      Start the development-only contract capture server")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Aliases: -h/--help print help; -v/--version print version metadata.")
	fmt.Fprintln(w, "Exit status: 0 success/help, 1 operational failure, 2 invalid usage.")
}
