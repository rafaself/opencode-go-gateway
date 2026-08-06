# OpenCode Gateway

OpenCode Gateway lets Codex CLI use models available through an OpenCode Go
subscription. The v0.1.0 path translates Codex Responses traffic to OpenCode
Go Chat Completions for `deepseek-v4-flash`, then translates the streamed
provider response back to Codex Responses events.

This is an independent local gateway, not OpenCode CLI. It does not replace,
invoke, or claim to be produced by OpenCode CLI, OpenAI, DeepSeek, or the Codex
CLI maintainers. It does not install a service, execute tool commands, or
apply patches to a user's filesystem.

## Architecture

```text
┌──────────────┐  Codex Responses/SSE  ┌─────────────────────┐  Chat Completions/SSE  ┌────────────────┐
│ Codex CLI    │ ────────────────────> │ OpenCode Gateway    │ ─────────────────────> │ OpenCode Go   │
│ user config  │ <──────────────────── │ decode → bridge    │ <───────────────────── │ deepseek-v4   │
└──────────────┘   Responses events   └──────────┬──────────┘   provider stream      └────────────────┘
                                                 │
                                      loopback health and safe logs
```

The gateway is local-first: Codex talks to the loopback listener, while the
OpenCode Go credential is used only for the outbound provider request.

## Prerequisites

- A Codex CLI installation. The checked compatibility baseline is Codex
  `0.146.0`.
- An OpenCode Go subscription and API key. Keep the key in the gateway process
  environment; never put it in Codex TOML or a checked-in file.
- For source builds: Go `1.22+`, GNU Make, and a POSIX shell.
- For the opt-in live smoke only: `curl`, `git`, `jq`, `rg`, Codex CLI, and
  network access. CI never sends a paid inference request.

## Install and run

For a release, download the archive for your OS/architecture, verify its
SHA-256 entry in `SHA256SUMS`, and extract it. Archives contain the binary,
`LICENSE`, and a minimal `README.txt`. The v0.1.0 release targets Linux
amd64/arm64, macOS amd64/arm64, and Windows amd64.

From source:

```bash
make build
export OPENCODE_GO_API_KEY='your-key'
./bin/opencode-gateway run
```

The default listener is `http://127.0.0.1:8787`. Health endpoints are:

```text
GET /health/live   -> 200 {"status":"ok"}
GET /health/ready  -> 200 {"status":"ready"}
POST /v1/responses -> streaming text/event-stream
```

The process stops cleanly on SIGINT/SIGTERM within the configured shutdown
deadline. `OPENCODE_GO_API_KEY` is required even when the first request has not
yet been made.

## CLI

```text
opencode-gateway run
opencode-gateway setup codex [--codex-home DIR] [--gateway-url URL] [--dry-run]
opencode-gateway setup codex --restore BACKUP_DIR
opencode-gateway doctor [--codex-home DIR] [--gateway-url URL]
opencode-gateway version
opencode-gateway help
opencode-gateway dev capture-codex ...       # development-only contract tool
```

`-h`/`--help` and `-v`/`--version` are aliases for the top-level help and
version commands. Exit status is stable: `0` means success or help, `1` means
an operational/diagnostic failure, and `2` means invalid command usage. A
failure message is written without request content, credentials, or raw
provider error bodies. Build metadata is normalized before it is printed.

`version` prints the release version, commit, UTC build date, and Go runtime.
Release builds inject those values with linker flags; source builds report
`dev`/`unknown` values. `help` prints the command summary and exit semantics.

## Configure Codex safely

```bash
./bin/opencode-gateway setup codex
./bin/opencode-gateway doctor
```

`setup codex` edits the user-level Codex home (`CODEX_HOME` when set, otherwise
the platform user home plus `.codex`). It never edits a project `.codex`
configuration. Use `--codex-home /absolute/path` for an isolated test home.
Setup creates a timestamped backup before a change, preserves unrelated TOML
settings/comments where safe, writes `config.toml` and `models.json` with
owner-only permissions, validates them, and replaces them atomically. It is
idempotent. `--dry-run` shows a redacted diff and writes nothing. Use the exact
printed backup path with `--restore` to roll back.

`doctor` reports `PASS`, `WARN`, and `FAIL`, and returns `1` when a required
check fails. It checks gateway configuration, the loopback port and health
endpoints, Codex TOML/catalog validity and permissions, Codex executable
version, provider connectivity, authentication, and
`deepseek-v4-flash` availability. It never prints the key. A missing key or
unavailable service is an actionable failure; a missing Codex executable or a
rate-limited optional provider check is reported distinctly.

The generated catalog contains the tested DeepSeek V4 Flash context,
reasoning, text-only, parallel-tool, `apply_patch` capability, compaction, and
transport metadata. It declares no WebSocket transport. Details and rollback
behavior are in [docs/codex-setup.md](docs/codex-setup.md).

### Manual configuration

If automation is unsuitable, add the equivalent managed values to the
user-level Codex configuration, adjusting the catalog path for the platform:

```toml
model = "deepseek-v4-flash"
model_provider = "opencode-gateway"
model_catalog_json = "/absolute/path/to/.codex/models.json"
model_reasoning_effort = "high"
model_supports_reasoning_summaries = false
model_reasoning_summary = "none"

[model_providers.opencode-gateway]
name = "OpenCode Gateway"
base_url = "http://127.0.0.1:8787/v1"
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
```

Do not add `OPENCODE_GO_API_KEY` or another bearer credential to this file.
The current Codex provider keys and profile rules are documented in
[docs/codex-setup.md](docs/codex-setup.md) and
[docs/codex-compatibility.md](docs/codex-compatibility.md).

## Configuration and protocol

The complete environment table, defaults, units, timeouts, and resource
limits are in [docs/configuration.md](docs/configuration.md). The supported
HTTP routes and the translated, explicit-no-op, deferred, and rejected issue
#2 fields are in [docs/protocol.md](docs/protocol.md). The detailed wire
contract, fixtures, response events, and continuation behavior are in:

- [Codex contract](docs/codex-contract.md)
- [Codex streaming](docs/codex-streaming.md)
- [OpenCode Go contract](docs/opencodego-contract.md)
- [checked-in fixtures](testdata/codex)

Before the first SSE event, errors are safe JSON envelopes with stable error
types. After streaming begins, a provider or gateway failure becomes exactly
one `response.failed` event. The gateway does not buffer a complete provider
response and does not emit `[DONE]`.

## Logs and troubleshooting

Logs contain operational metadata such as component, status, bounded timing,
and request correlation. They do not contain prompts, instructions, source
code, filesystem paths, environment values, credentials, authorization
headers, tool arguments/results, or provider reasoning. If a diagnostic output
contains sensitive material, stop and remove it before sharing.

Common checks:

| Symptom | Check |
| --- | --- |
| `OPENCODE_GO_API_KEY is required` | Export the key in the gateway process environment only |
| Codex cannot connect | Start `run`, verify `/health/live`, and confirm the provider `base_url` ends in `/v1` |
| `doctor` reports config/catalog failure | Run `setup codex --dry-run`, inspect the isolated home, then apply setup or restore its backup |
| Provider authentication/model failure | Run `doctor` with the key present; do not paste the credential into a report |
| Stream ends as `response.failed` | Inspect safe gateway status logs and provider availability; retry only from Codex as appropriate |
| Tool continuation is unknown/expired | Restart the request from a fresh Codex turn; continuation state is process-local and bounded |

## Limitations and safety

This release supports the tested streaming Responses subset and standard
function tools, parallel calls, Codex's `apply_patch` event shape, and bounded
tool-result continuation. It does not implement the complete Responses API,
non-streaming Responses, arbitrary custom/deferred tools, generic web search,
forced/named provider tool choices, or filesystem patch execution.

`namespace` and `web_search` metadata from the captured #2 contract is accepted
only in the exact observed shape and omitted from the provider request. Unknown
fields and unsupported capabilities fail closed. The service is loopback-only
by default, has no automatic retries, finite request/stream limits, and no
public authentication layer. Read [docs/security.md](docs/security.md) before
enabling non-loopback binding.

## Uninstall and rollback

Stop the gateway and remove the extracted binary/archive. For Codex settings,
use the backup path printed by setup:

```bash
./bin/opencode-gateway setup codex --restore \
  /absolute/path/to/backup-opencode-gateway-...
```

Alternatively, remove only the managed `opencode-gateway` provider and
`models.json` after taking a backup. Setup does not install a service and the
gateway has no persistent runtime database.

## Development, compatibility, and release

The project uses only the Go standard library in production. Normal checks:

```bash
go fmt ./...
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build -trimpath ./cmd/opencode-gateway
git diff --check
```

Contract/integration/fuzz checks are available through `make contract`,
`make integration`, and `make fuzz-smoke FUZZTIME=1s`. `make release-check`
adds all required release gates; `make package-self-test` builds and verifies
the cross-platform archives locally.

Read [docs/architecture.md](docs/architecture.md) for contribution and
release checklists, [docs/codex-compatibility.md](docs/codex-compatibility.md)
for safe fixture recapture, and [docs/release.md](docs/release.md) for the
isolated RC procedure and opt-in paid smoke policy. The v0.1.0 release notes
are in [docs/releases/v0.1.0.md](docs/releases/v0.1.0.md).
