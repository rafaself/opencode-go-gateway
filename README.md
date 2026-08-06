# opencode-go-gateway

This repository is a standard-library-first Go gateway. The current vertical
slice accepts streaming, text-only Codex Responses requests and forwards them
to OpenCode Go's `deepseek-v4-flash` Chat Completions stream. The development-
only Codex Responses capture server remains available for contract work.

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

The runtime requires `OPENCODE_GO_API_KEY` for the OpenCode Go upstream and
binds to `127.0.0.1:8787` by default:

```bash
export OPENCODE_GO_API_KEY='your-opencode-go-key'
./bin/opencode-gateway run
```

`./bin/opencode-gateway version` prints the version, commit, build date, and Go
runtime metadata. Configuration is loaded from these environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENCODE_GO_API_KEY` | required | OpenCode Go credential; never logged |
| `OPENCODE_GO_BASE_URL` | `https://opencode.ai/zen/go/v1` | OpenCode Go base URL |
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

The HTTP contract for the text-only milestone is:

```text
GET  /health/live   -> 200 {"status":"ok"}
GET  /health/ready  -> 200 {"status":"ready"}
POST /v1/responses  -> 200 text/event-stream (text-only requests)
                         4xx/5xx JSON error envelope before streaming
                         501 feature_not_implemented for tools/continuation
```

Unknown paths return a JSON `not_found` error and unsupported methods return a
JSON `method_not_allowed` error with an `Allow` header. Responses are emitted
incrementally as SSE bytes; the gateway does not buffer the provider response.
SIGINT and SIGTERM stop accepting new connections and use the configured
bounded shutdown grace period. Structured logs contain request metadata only;
bodies, credentials, prompts, source code, reasoning content, and tool data are
not logged.

## First text-only smoke test

The real-provider smoke test is opt-in because it makes a paid/network request.
It uses a temporary Codex home and Git repository, keeps the OpenCode Go key in
the gateway process environment only, and never prints the key. Build the
binary first, then run:

```bash
make build
export OPENCODE_GO_API_KEY='your-opencode-go-key'
RUN_LIVE_SMOKE=1 ./scripts/live-smoke.sh
```

The script requires `codex`, `curl`, `git`, `jq`, and `rg`. It checks that Codex
emits a non-empty `agent_message` event (`item.started`, `item.updated`,
`item.completed`, or the supported `item.delta` form) while the Codex process is
still live, then ends with `turn.completed`. It also checks that the gateway
records the `deepseek-v4-flash` model and `response.completed` terminal event,
that the prompt marker and API key are absent from gateway logs, and that the
key is absent from the temporary Codex home and repository. The process-liveness
check is intentionally stronger than counting JSONL lines, but shell/FIFO
polling cannot prove the exact wire-level timestamp at which Codex generated an
event. The offline event-shape regression test is
`./scripts/live-smoke-test.sh`.

No live smoke request runs in CI. Failed runs retain their temporary directory
and print its path for diagnostics; successful runs remove it automatically. For
a length limit, the terminal event is `response.incomplete`; a provider or
transport failure after SSE starts is `response.failed`.

This milestone intentionally rejects tools, tool results, and continuation
state with a stable `feature_not_implemented` response. Function tools,
custom tools, continuation, setup, and doctor flows belong to later issues.

## Capture the Codex contract

Build or run the development-only capture server with Go 1.22 or newer:

```bash
./bin/opencode-gateway dev capture-codex \
  --name simple \
  --output-dir /tmp/opencode-codex-capture
```

The server prints a loopback-only `base_url` for a Codex custom provider. Configure Codex with that URL and `wire_api = "responses"`, then run a request. Each request is written as a redacted JSON fixture. The default response is a minimal text SSE stream; `--response function`, `--response parallel`, and `--response custom` exercise tool-call shapes.

The captured contract and recapture instructions are documented in [docs/codex-contract.md](docs/codex-contract.md). The checked-in fixtures are under [testdata/codex](testdata/codex).

Run only the capture and contract validation suite with:

```bash
go test ./internal/capture
```
