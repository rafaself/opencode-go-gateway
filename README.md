# opencode-go-gateway

This repository currently establishes the Codex Responses wire contract before protocol translation is added.

## Capture the Codex contract

Build or run the development-only capture server with Go 1.22 or newer:

```bash
go run ./cmd/opencode-gateway dev capture-codex \
  --name simple \
  --output-dir /tmp/opencode-codex-capture
```

The server prints a loopback-only `base_url` for a Codex custom provider. Configure Codex with that URL and `wire_api = "responses"`, then run a request. Each request is written as a redacted JSON fixture. The default response is a minimal text SSE stream; `--response function`, `--response parallel`, and `--response custom` exercise tool-call shapes.

The captured contract and recapture instructions are documented in [docs/codex-contract.md](docs/codex-contract.md). The checked-in fixtures are under [testdata/codex](testdata/codex).

Run the validation suite with:

```bash
go test ./...
```
