# Protocol support matrix

OpenCode Gateway v0.1.0 is a deliberately narrow compatibility adapter. It
accepts the tested Codex Responses streaming contract and translates it to
OpenCode Go Chat Completions for `deepseek-v4-flash`; it is not a general
OpenAI Responses server.

## HTTP surface

| Request | Success | Error behavior |
| --- | --- | --- |
| `GET /health/live` | `200 application/json`, `{"status":"ok"}` | Method/path errors use the safe JSON error envelope |
| `GET /health/ready` | `200 application/json`, `{"status":"ready"}` while serving | `503` while shutting down |
| `POST /v1/responses` with `Content-Type: application/json` and `stream: true` | `200 text/event-stream` with Codex Responses events | Safe JSON error before the first SSE event; one `response.failed` after streaming begins |
| Any other path or method | — | `404 not_found` or `405 method_not_allowed`; `Allow` is set for method errors |

The gateway owns downstream response IDs, event ordering, indexes, terminal
state, and bounded stream lifetimes. It does not emit a `[DONE]` marker. See
[Codex streaming](codex-streaming.md) for event-state details and
[the provider contract](opencodego-contract.md) for the outbound mapping.

## Request field policy from issue #2

`testdata/codex/field-policy.json` is the executable policy. A field not
classified there is rejected by the decoder; a field marked `defer` is not
silently treated as supported.

| Area | Translate | Accept as explicit no-op | Defer or reject |
| --- | --- | --- | --- |
| Top-level | `model`, `instructions`, `input`, `tools`, `tool_choice`, `parallel_tool_calls`, `reasoning`, `text` when observed and tested, `stream`, `include`, `previous_response_id` | `stream_options`, `store`, `service_tier`, `prompt_cache_key`, `metadata`, `client_metadata` | `background`; unknown fields |
| Input items | `message`, `function_call`, `function_call_output`, `custom_tool_call`, `custom_tool_call_output` | — | Unknown item types; incomplete or mismatched continuation items |
| Tool declarations | `function`; the observed `custom`/`apply_patch` shape through its request-scoped registry | Observed `namespace` and `web_search` metadata are retained as explicit non-executing metadata and omitted from the provider request | Unknown tool types; unsupported custom/deferred tools |
| Tool choice and output | `auto`, `none`, and the tested parallel policy | — | Forced/named choices and unsupported structured output formats |

The distinction between `defer` and no-op is intentional: `namespace` and
`web_search` are accepted only in the exact observed metadata shape from #2,
then omitted because this release cannot execute them. Other deferred or
unknown capabilities fail closed. `stream` must be present and `true`; a
non-streaming Responses request is outside this release.

## Translation boundaries

| Codex concept | OpenCode Go representation | Boundary |
| --- | --- | --- |
| Instructions and messages | Ordered Chat Completions `system`, `user`, and `assistant` messages | Provider has no separate developer role, so system/developer instructions remain ordered system messages |
| Function declarations | Provider function tools with bounded raw JSON Schema | Names and aggregate schemas are validated before the provider call |
| Parallel function calls | One assistant tool-call message with original indexes and IDs | Fragmented arguments are accumulated under finite limits |
| `apply_patch` | Private `__ocg_apply_patch` function wrapper | Codex sees public custom-tool events; the gateway never executes or validates filesystem changes |
| Tool results | Provider `tool` messages and bounded continuation state | Results must match the pending provider turn; restart loses state |
| Provider reasoning | Private stream/continuation metadata | Not emitted as Codex reasoning text or logged |

The checked-in fixtures under `testdata/codex` and the contract tests are the
authoritative executable examples. When Codex changes its request or event
shape, follow the safe recapture procedure in
[codex-compatibility.md](codex-compatibility.md) and update the policy before
accepting a new field.
