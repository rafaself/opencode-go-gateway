# opencode-go-gateway

This repository is a standard-library-first Go gateway. The current M1 slice
provides a local HTTP lifecycle and the development-only Codex Responses
capture server; protocol translation is introduced by later milestones.

## Requirements and developer commands

Use Go 1.22 or newer. The repository has no production dependencies outside
the Go standard library.

```bash
make fmt
make vet
make test
make race
make build
```

The resulting binary is `bin/opencode-gateway`.
The `make build` target injects the Git-derived version and commit plus the
UTC build timestamp; `VERSION`, `COMMIT`, and `BUILD_DATE` can be overridden
for release builds.

## Run the local gateway

The runtime requires `OPENCODE_GO_API_KEY` even though the M1 placeholder does
not make an upstream request. It binds to `127.0.0.1:8787` by default:

```bash
export OPENCODE_GO_API_KEY='your-opencode-go-key'
opencode-gateway run
```

`opencode-gateway version` prints the version, commit, build date, and Go
runtime metadata. Configuration is loaded from these environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENCODE_GO_API_KEY` | required | OpenCode Go credential; never logged |
| `OPENCODE_GO_BASE_URL` | `https://opencode.ai/zen/go/v1` | future upstream base URL |
| `OPENCODE_GATEWAY_HOST` | `127.0.0.1` | local bind host |
| `OPENCODE_GATEWAY_PORT` | `8787` | local bind port; `0` selects an ephemeral port |
| `OPENCODE_GATEWAY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `OPENCODE_GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | bounded graceful shutdown |
| `OPENCODE_GATEWAY_READ_HEADER_TIMEOUT` | `5s` | HTTP header deadline |
| `OPENCODE_GATEWAY_READ_TIMEOUT` | `30s` | HTTP read deadline |
| `OPENCODE_GATEWAY_WRITE_TIMEOUT` | `30s` | HTTP write deadline |
| `OPENCODE_GATEWAY_IDLE_TIMEOUT` | `60s` | HTTP idle connection deadline |
| `OPENCODE_GATEWAY_MAX_BODY_BYTES` | `16777216` | request body limit |
| `OPENCODE_GATEWAY_MAX_HEADER_BYTES` | `65536` | request header limit |
| `OPENCODE_GATEWAY_ALLOW_NON_LOOPBACK` | `false` | explicit opt-in for non-loopback binding |

The HTTP contract for this milestone is:

```text
GET  /health/live   -> 200 {"status":"ok"}
GET  /health/ready  -> 200 {"status":"ready"}
POST /v1/responses  -> 501 {"error":{"type":"not_implemented", ...}}
```

Unknown paths return a JSON `not_found` error and unsupported methods return a
JSON `method_not_allowed` error with an `Allow` header. SIGINT and SIGTERM
stop accepting new connections and use the configured bounded shutdown grace
period. Structured logs contain request metadata only; bodies, credentials,
prompts, source code, and tool data are not logged.

## Capture the Codex contract

Build or run the development-only capture server with Go 1.22 or newer:

```bash
go run ./cmd/opencode-gateway dev capture-codex \
  --name simple \
  --output-dir /tmp/opencode-codex-capture
```

The server prints a loopback-only `base_url` for a Codex custom provider. Configure Codex with that URL and `wire_api = "responses"`, then run a request. Each request is written as a redacted JSON fixture. The default response is a minimal text SSE stream; `--response function`, `--response parallel`, and `--response custom` exercise tool-call shapes.

The captured contract and recapture instructions are documented in [docs/codex-contract.md](docs/codex-contract.md). The checked-in fixtures are under [testdata/codex](testdata/codex).

Run only the capture and contract validation suite with:

```bash
go test ./internal/capture
```
