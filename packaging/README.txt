OpenCode Gateway release archive

This archive contains the OpenCode Gateway binary, the MIT license, and this
minimal installation note. The full documentation is available at:
https://github.com/rafaself/opencode-go-gateway

The gateway lets Codex CLI use models available through an OpenCode Go
subscription. Keep OPENCODE_GO_API_KEY in the gateway process environment; do
not put it in Codex configuration files.

Quick start:

  export OPENCODE_GO_API_KEY="your-key"
  ./opencode-gateway run

Then configure Codex with `opencode-gateway setup codex` and run
`opencode-gateway doctor`. Use `opencode-gateway version` to inspect the
embedded release metadata.
