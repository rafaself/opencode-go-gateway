# Runtime configuration

`opencode-gateway run` reads operational configuration from the process
environment. It does not read a dotenv file, a Codex config, or a project
configuration file. `OPENCODE_GO_API_KEY` is preferred when present; when it
is absent, `ocgtw run` loads the credential saved by `ocgtw config set-key`.
The key remains confined to the gateway process and upstream request; it is
never sent to Codex or included in logs.

The defaults below are the v0.1.0 contract. Duration values use Go duration
syntax such as `500ms`, `10s`, or `2m`. Byte values are decimal integer byte
counts, and item/request values are integer counts. `0` is accepted only for
the gateway port, where it requests an ephemeral listener for tests or local
embedding.

## Provider and listener

| Variable | Default | Unit | Meaning |
| --- | ---: | --- | --- |
| `OPENCODE_GO_API_KEY` | optional when stored | string | OpenCode Go credential; environment value takes precedence and is never printed |
| `OPENCODE_GO_BASE_URL` | `https://opencode.ai/zen/go/v1` | URL | OpenCode Go Chat Completions base URL |
| `OPENCODE_GO_MODEL` | `deepseek-v4-flash` | enum | Go backend upstream model; accepts `deepseek-v4-flash`, `deepseek-v4-pro`, or `deepseek-v4-flash-free` |
| `OPENCODE_GO_ZEN_BASE_URL` | `https://opencode.ai/zen/v1` | URL | OpenCode Zen Chat Completions base URL |
| `OPENCODE_GO_ZEN_MODEL` | `deepseek-v4-flash-free` | enum | Zen backend upstream model; accepts `deepseek-v4-flash`, `deepseek-v4-pro`, or `deepseek-v4-flash-free` |
| `OPENCODE_GATEWAY_HOST` | `127.0.0.1` | host | Local bind host |
| `OPENCODE_GATEWAY_PORT` | `8787` | port | Local bind port; `0` selects an ephemeral port |
| `OPENCODE_GATEWAY_ALLOW_NON_LOOPBACK` | `false` | boolean | Explicitly permits a non-loopback bind; use only with a separately secured network boundary |
| `OPENCODE_GATEWAY_LOG_LEVEL` | `info` | enum | `debug`, `info`, `warn`, or `error` |

One gateway instance serves both backends. Each Responses request must name a
tagged model, `"<label> (go)"` or `"<label> (zen)"`; the tag selects the
backend and the label is client metadata that is never forwarded. An untagged
model is rejected with `400 invalid_request`. The SSE response echoes the
selected backend's upstream model.

The provider URL must be absolute HTTPS. A loopback HTTP URL is allowed for
local deterministic tests. Redirects and ambient proxy settings are disabled.
The listener is loopback-only unless the explicit non-loopback opt-in is set;
that opt-in does not add authentication or CORS policy.

## Timeouts

| Variable | Default | Unit | Applies to |
| --- | ---: | --- | --- |
| `OPENCODE_GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | duration | Graceful shutdown |
| `OPENCODE_GATEWAY_READ_HEADER_TIMEOUT` | `5s` | duration | HTTP request headers |
| `OPENCODE_GATEWAY_REQUEST_BODY_READ_TIMEOUT` | `30s` | duration | Request body phase |
| `OPENCODE_GATEWAY_IDLE_TIMEOUT` | `60s` | duration | Idle HTTP connections |
| `OPENCODE_GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | `10s` | duration | Provider TCP connection |
| `OPENCODE_GATEWAY_TLS_HANDSHAKE_TIMEOUT` | `10s` | duration | Provider TLS handshake |
| `OPENCODE_GATEWAY_RESPONSE_HEADER_TIMEOUT` | `30s` | duration | Provider response headers |
| `OPENCODE_GATEWAY_STREAM_IDLE_TIMEOUT` | `60s` | duration | Silence between provider bytes, including the first byte |
| `OPENCODE_GATEWAY_DOWNSTREAM_WRITE_TIMEOUT` | `30s` | duration | Each downstream SSE write or flush |

There is deliberately no total generation timeout. A healthy stream may run
longer than these phase deadlines while it continues producing bytes.

## Request and stream limits

| Variable | Default | Unit | Applies to |
| --- | ---: | --- | --- |
| `OPENCODE_GATEWAY_MAX_BODY_BYTES` | `16777216` | bytes | Incoming request body |
| `OPENCODE_GATEWAY_MAX_HEADER_BYTES` | `65536` | bytes | HTTP request headers |
| `OPENCODE_GATEWAY_MAX_INPUT_ITEMS` | `256` | items | Responses input items |
| `OPENCODE_GATEWAY_MAX_COLLECTION_ITEMS` | `256` | items | JSON object members or array elements |
| `OPENCODE_GATEWAY_MAX_TOOLS` | `128` | tools | Declared request tools |
| `OPENCODE_GATEWAY_MAX_SCHEMA_BYTES` | `262144` | bytes | Aggregate provider-visible JSON Schemas |
| `OPENCODE_GATEWAY_MAX_SSE_LINE_BYTES` | `262144` | bytes | One upstream SSE line |
| `OPENCODE_GATEWAY_MAX_SSE_EVENT_BYTES` | `4194304` | bytes | One upstream SSE event |
| `OPENCODE_GATEWAY_MAX_SSE_BUFFERED_BYTES` | `8388608` | bytes | Buffered/retained provider stream data |
| `OPENCODE_GATEWAY_MAX_SSE_READ_BUFFER_BYTES` | `32768` | bytes | SSE decoder read buffer |
| `OPENCODE_GATEWAY_MAX_OUTPUT_BYTES` | `16777216` | bytes | Retained output and tool arguments |
| `OPENCODE_GATEWAY_MAX_TEXT_BYTES` | `8388608` | bytes | Visible text |
| `OPENCODE_GATEWAY_MAX_REASONING_BYTES` | `8388608` | bytes | Retained provider reasoning metadata |
| `OPENCODE_GATEWAY_MAX_TOOL_CALL_ARGUMENT_BYTES` | `1048576` | bytes | One tool-call argument |
| `OPENCODE_GATEWAY_MAX_ACTIVE_REQUESTS` | `64` | requests | Concurrent requests; excess requests receive `429` |

## Continuation retention limits

Tool-result continuation state is process-local and bounded. A restart loses
pending state and requires Codex to retry from a fresh provider turn.

| Variable | Default | Unit | Applies to |
| --- | ---: | --- | --- |
| `OPENCODE_GATEWAY_MAX_PENDING_TURN_BYTES` | `16777216` | bytes | One retained continuation |
| `OPENCODE_GATEWAY_MAX_PENDING_RECORDS` | `128` | records | Pending continuation count |
| `OPENCODE_GATEWAY_MAX_PENDING_AGGREGATE_BYTES` | `134217728` | bytes | All retained continuations |

The continuation store has its own finite pending TTL, consuming lease, and
short consumed grace period. Those lifecycle values are implementation
defaults rather than environment variables; see
[the provider contract](opencodego-contract.md#tool-result-continuation-state).

## Safe configuration workflow

Build metadata is injected only at build time. The supported commands and
their exit semantics are documented in the [CLI and release guide](release.md).
Use `ocgtw config status` to inspect credential state without printing a
credential, and use `ocgtw config remove-key` to delete the stored value.

On Linux, `config set-key` prefers the Secret Service keyring. If no keyring
helper is available, it uses the per-user configuration directory with a
`0700` directory and `0600` file. That fallback protects the file from other
ordinary users but is not encrypted at rest; use the keyring or an environment
variable when disk-level compromise is in scope. The key is never accepted as
a command-line argument. For Codex user configuration, use
[`setup codex`](codex-setup.md); never copy the provider key into TOML.
