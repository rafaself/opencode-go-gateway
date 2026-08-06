# opencode-go-gateway

This repository is a standard-library-first Go gateway. The current vertical
slice accepts streaming Codex Responses requests with standard function tools
and forwards them to OpenCode Go's `deepseek-v4-flash` Chat Completions stream.
The development-only Codex Responses capture server remains available for
contract work.

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
| `OPENCODE_GATEWAY_IDLE_TIMEOUT` | `60s` | HTTP idle connection deadline |
| `OPENCODE_GATEWAY_REQUEST_BODY_READ_TIMEOUT` | `30s` | request-body read phase |
| `OPENCODE_GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | `10s` | upstream TCP connect phase |
| `OPENCODE_GATEWAY_TLS_HANDSHAKE_TIMEOUT` | `10s` | upstream TLS handshake phase |
| `OPENCODE_GATEWAY_RESPONSE_HEADER_TIMEOUT` | `30s` | upstream response-header phase |
| `OPENCODE_GATEWAY_STREAM_IDLE_TIMEOUT` | `60s` | maximum silence between upstream bytes, including the first byte |
| `OPENCODE_GATEWAY_DOWNSTREAM_WRITE_TIMEOUT` | `30s` | each downstream SSE write/flush |
| `OPENCODE_GATEWAY_MAX_BODY_BYTES` | `16777216` | request body limit |
| `OPENCODE_GATEWAY_MAX_HEADER_BYTES` | `65536` | request header limit |
| `OPENCODE_GATEWAY_MAX_INPUT_ITEMS` | `256` | input item count limit |
| `OPENCODE_GATEWAY_MAX_COLLECTION_ITEMS` | `256` | JSON object-member and array-element limit |
| `OPENCODE_GATEWAY_MAX_TOOLS` | `128` | declared tool count limit |
| `OPENCODE_GATEWAY_MAX_SCHEMA_BYTES` | `262144` | aggregate provider schema limit, including the implicit apply_patch wrapper |
| `OPENCODE_GATEWAY_MAX_SSE_LINE_BYTES` | `262144` | one upstream SSE line limit |
| `OPENCODE_GATEWAY_MAX_SSE_EVENT_BYTES` | `4194304` | one upstream SSE event limit |
| `OPENCODE_GATEWAY_MAX_SSE_BUFFERED_BYTES` | `8388608` | upstream SSE/retained stream limit |
| `OPENCODE_GATEWAY_MAX_SSE_READ_BUFFER_BYTES` | `32768` | SSE decoder read buffer size |
| `OPENCODE_GATEWAY_MAX_OUTPUT_BYTES` | `16777216` | visible output and tool-argument limit |
| `OPENCODE_GATEWAY_MAX_TEXT_BYTES` | `8388608` | visible text limit |
| `OPENCODE_GATEWAY_MAX_REASONING_BYTES` | `8388608` | retained reasoning metadata limit |
| `OPENCODE_GATEWAY_MAX_TOOL_CALL_ARGUMENT_BYTES` | `1048576` | one tool-argument limit |
| `OPENCODE_GATEWAY_MAX_PENDING_TURN_BYTES` | `16777216` | one retained continuation limit |
| `OPENCODE_GATEWAY_MAX_PENDING_RECORDS` | `128` | retained continuation record-count limit |
| `OPENCODE_GATEWAY_MAX_PENDING_AGGREGATE_BYTES` | `134217728` | retained continuation aggregate-byte limit |
| `OPENCODE_GATEWAY_MAX_ACTIVE_REQUESTS` | `64` | concurrent request limit; overflow returns `429` |
| `OPENCODE_GATEWAY_ALLOW_NON_LOOPBACK` | `false` | explicit opt-in for non-loopback binding |

The local listener and request `Host`/`Origin` are loopback-only by default.
Non-loopback binding requires the explicit opt-in above; requests still receive
no permissive CORS headers. The provider URL is validated as an HTTPS URL (or a
loopback HTTP URL for local tests), redirects are disabled, and the transport
does not use ambient proxy settings. The gateway has no automatic retries: a
failed request or stream is attempted once. The checked-in generated v0.1
profile at [`profiles/codex-v0.1.toml`](profiles/codex-v0.1.toml) targets the
default loopback address and sets both request and stream maximum retries to
`0`.

Errors before the first downstream SSE frame are JSON envelopes with stable
`error.type` taxonomy, provider/detail `error.code`, `error.param`, and a safe
`error.message`. After SSE begins, provider and phase failures are represented
by exactly one `response.failed`; the gateway never falls back to a second JSON
error. `Retry-After` is forwarded only when it is a valid numeric delay or HTTP
date. The timeout settings are phase-specific; no total generation timeout is
applied while an active stream continues to produce data.

The HTTP contract for the function-tool milestone is:

```text
GET  /health/live   -> 200 {"status":"ok"}
GET  /health/ready  -> 200 {"status":"ready"}
POST /v1/responses  -> 200 text/event-stream (messages and function tools)
                         plus Codex's apply_patch custom-tool event shape
                         plus function/custom tool-result continuations
                         4xx/5xx JSON error envelope before streaming
                         501 feature_not_implemented for unsupported custom
                             tools and other unsupported Responses features
```

Unknown paths return a JSON `not_found` error and unsupported methods return a
JSON `method_not_allowed` error with an `Allow` header. Responses are emitted
incrementally as SSE bytes; the gateway does not buffer the provider response.
SIGINT and SIGTERM stop accepting new connections and use the configured
bounded shutdown grace period. Structured logs contain request metadata only;
bodies, credentials, prompts, source code, reasoning content, and tool data are
not logged.

## Configure and diagnose Codex

The binary can safely configure the user-level Codex provider and generate the
DeepSeek V4 Flash model catalog:

```bash
./bin/opencode-gateway setup codex
./bin/opencode-gateway doctor
```

Setup creates a timestamped backup before changing `config.toml`, preserves
unrelated settings/comments where safe, writes owner-only files atomically,
and never stores `OPENCODE_GO_API_KEY`. Use `--dry-run` for a redacted preview
or restore a printed backup with `setup codex --restore <backup-directory>`.
The full path resolution, catalog schema, rollback, and doctor check behavior
are documented in [docs/codex-setup.md](docs/codex-setup.md).

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

This milestone accepts standard function definitions with bounded JSON Schema
parameters, reconstructs fragmented/parallel provider tool calls, and adapts
Codex's implicit freeform `apply_patch` tool through a request-scoped strict
function wrapper. Thinking mode omits upstream `tool_choice: auto` for
DeepSeek compatibility. The gateway preserves exact custom input and result
text but never executes or validates filesystem effects; forced or named
choices and unsupported custom tools return explicit unsupported/invalid
responses. Finalized DeepSeek reasoning/tool-call turns are retained only in a
bounded in-memory store for the next matching function/custom result request;
unknown, expired, mixed, incomplete, duplicate, or mismatched results return
stable continuation errors. A process restart intentionally loses that
temporary state and requires the client to retry from a fresh tool turn.

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
