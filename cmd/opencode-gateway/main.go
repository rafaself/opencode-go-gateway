package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/rafaself/opencode-go-gateway/internal/capture"
)

func main() {
	if len(os.Args) < 3 || os.Args[1] != "dev" || os.Args[2] != "capture-codex" {
		usage(os.Stderr)
		os.Exit(2)
	}

	if err := runCapture(os.Args[3:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-gateway: %v\n", err)
		os.Exit(1)
	}
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
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	mode, err := capture.ParseResponseMode(*responseMode)
	if err != nil {
		return err
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: opencode-gateway dev capture-codex [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Start a loopback-only Codex Responses contract capture server.")
	fmt.Fprintln(w, "Run `opencode-gateway dev capture-codex -h` for flags.")
}
