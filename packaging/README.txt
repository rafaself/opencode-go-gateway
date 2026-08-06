OpenCode Gateway release archive

This archive contains the OpenCode Gateway binaries (`opencode-gateway` and
the shorter `ocgtw` name), the MIT license, and this minimal installation
note. The full documentation is available at:
https://github.com/rafaself/opencode-go-gateway

The gateway lets Codex CLI use models available through an OpenCode Go
subscription. Configure the credential with `ocgtw config set-key` or keep
OPENCODE_GO_API_KEY in the gateway process environment; do not put it in Codex
configuration files.

Quick start:

  ./ocgtw config set-key
  ./ocgtw run

The command reads the key from a hidden terminal prompt. For a non-interactive
shell, pipe one line to `ocgtw config set-key --stdin` without putting the key
in an argument. Then configure Codex with `ocgtw setup codex` and run
`ocgtw doctor`. Use `ocgtw version` to inspect the embedded release metadata.
