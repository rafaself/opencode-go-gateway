# OpenCode Go Chat Completions client

`internal/opencodego` is the outbound provider boundary for the Chat
Completions client and its temporary M8 continuation state. It owns the Chat
Completions wire structs and translates only the validated provider-neutral
values in `internal/bridge`. It does not import `internal/codex`, execute
tools, or expose provider reasoning as bridge text.

The shipped provider defaults are:

- base URL: `https://opencode.ai/zen/go/v1`;
- model: `deepseek-v4-flash`;
- endpoint: `POST /chat/completions` relative to that base URL;
- streaming: `stream: true`;
- response media type: `text/event-stream`.

The provider model policy is explicit: `deepseek-v4-flash` is the default and
`deepseek-v4-pro` is the only other configured model accepted by this MVP.
Unsupported configured provider models fail before any network request. The
incoming bridge model (for example, Codex's `gpt-5.3-codex`) is source
metadata and is intentionally not forwarded as the OpenCode Go model.

The OpenCode Go documentation lists the same model ID and Chat Completions
endpoint family in its [Go endpoint table](https://opencode.ai/docs/go/). The
[DeepSeek Chat Completions reference](https://api-docs.deepseek.com/api/create-chat-completion/)
defines the message, function-tool, thinking, tool-choice, and streaming
fields used here.

## Bridge-to-provider mapping

`Client.Do(ctx, bridge.Request)` prepends a non-empty `Request.Instructions`
as one `system` message and then preserves the order of bridge input items.
Bridge `system` and `developer` messages both become provider `system`
messages because the provider contract has no `developer` role; they remain
separate messages so boundaries and ordering are not silently collapsed.
Multiple text parts in one bridge message are joined with a single newline in
their original order because the provider MVP accepts string message content.

The tool history mapping is explicit:

| Bridge item | Chat Completions message |
| --- | --- |
| `Message` | `system`, `user`, or `assistant` message |
| contiguous `FunctionCall`/`CustomToolCall` items | one `assistant` message with `tool_calls` entries in original order |
| `FunctionCallOutput` | `tool` message with `tool_call_id` |
| `CustomToolCallOutput` | `tool` message with `tool_call_id` |

`CustomToolCall` is mapped only when the request carries its request-scoped
`bridge.ToolRegistry`. The Codex `apply_patch` name is wrapped as the
provider function `__ocg_apply_patch` with one strict string property named
`input`; the decoded value is not interpreted or normalized. `CustomToolCall`
and `CustomToolCallOutput` retain their original call ID so a later
continuation can reconstruct the provider tool turn.

Contiguous tool-call inputs form one assistant message. A normal message or a
tool result ends the group, and tool results remain separate messages in their
original order. This preserves the Chat Completions conversation shape and
gives #10 a stable grouping boundary when reconstructing parallel calls. Each
function name is validated against the provider's ASCII `[A-Za-z0-9_-]`
contract and the 64-byte maximum before the request is sent.

The provider request supports function declarations only. Function schemas are
copied as raw validated JSON under the provider's `{ "type": "function", "function": ... }`
wrapper. The implicit Codex `apply_patch` capability uses the same provider
function wire shape through the synthetic wrapper described above. When a
request registry is attached, the exact #2 `mcp` namespace and standalone
web-search declarations are accepted as metadata and omitted from the
provider request; they are never executed or treated as generic plugins.
Direct mapping without that registry still rejects deferred tools. JSON-schema
response formatting is also rejected because the provider MVP supports only
text and `json_object` output formats.

## Thinking and tool-choice policy

The provider documents thinking as enabled by default and accepts
`reasoning_effort` values `high` and `max`. Its compatibility values map as
follows for both supported MVP models:

| Bridge effort | Provider effort |
| --- | --- |
| `low` | `high` |
| `medium` | `high` |
| `high` | `high` |
| `xhigh` | `max` |
| `max` | `max` |

Unknown efforts are rejected.

The client sends the provider `thinking` extension explicitly. In thinking
mode, `tool_choice: auto` is omitted so the provider's documented default can
select tools, and `none` is preserved. Forced (`required`) and named choices
are rejected explicitly in this milestone in every thinking mode; they are
never silently rewritten to `auto`. A reasoning effort combined with disabled
thinking is rejected.

Function tools are bounded to 128 declarations and 256 KiB of aggregate raw
JSON Schema bytes. The stream adapter bounds each accumulated call argument to
1 MiB and the complete retained stream to its configured aggregate limit; the
`apply_patch` freeform input has an exclusive 512 KiB ceiling: lengths at or
above 512 KiB are rejected, so the largest accepted value is 512 KiB minus one
byte. Schemas and model argument strings are transported without semantic
rewriting; invalid model JSON is left for Codex/tool execution to handle. This
gateway never executes or validates filesystem effects for `apply_patch`.

Provider `reasoning_content` is present in `ChatCompletionResponse` and
`ChatCompletionChunk` message structs. It remains provider metadata for the
SSE/continuation boundary; this package never turns it into bridge text or a
Responses summary. DeepSeek requires that value to be carried through later
tool-call turns, so the M8 continuation store retains it only until its
bounded TTL. Stream choice indexes and each tool-call delta's `index` are
retained, including index zero, so #6 can reconstruct interleaved parallel
calls without guessing positions.

`parallel_tool_calls` is always emitted as an explicit boolean, including
`false`, to preserve the bridge request's generation policy.

## Tool-result continuation state

Issue #10 adds an in-memory continuation store owned by `internal/opencodego`.
When a finalized provider stream ends with one or more tool calls, the adapter
retains only the provider/model identity, non-public `reasoning_content`,
compatible assistant content, original provider tool calls and registrations,
call IDs, and bounded lifecycle metadata. It does not retain the conversation
or write reasoning, prompts, source, or tool output to logs.

The next Responses request must contain every matching
`function_call_output`/`custom_tool_call_output` item for that assistant turn.
Results may arrive in any order; the reconstructed Chat Completions request
contains exactly one assistant message with all original tool calls in order,
followed by one `role: tool` message per result in request order. Custom
`apply_patch` calls are replayed through the same private
`__ocg_apply_patch` wrapper used by #9, while Codex continues to see the public
`apply_patch` name. Empty output is preserved as an empty, non-null provider
message. Error/status markers remain semantic bridge metadata while the exact
textual output is passed unchanged to the provider.

The store uses explicit `pending`, `consuming`, `consumed`, and `expired`
states. A result request reserves a lease; the lease is committed only after
the first valid provider response-start event has been translated and handled
by the Responses stream. Header validation alone is not acceptance. The lease
is aborted on cancellation, transport failure, malformed/empty streams, or an
upstream 4xx before that event, allowing a retry. Once response-start is
accepted, later provider failures retain consumed semantics.
Concurrent submissions receive a stable busy error, and duplicate submissions
after acceptance receive a consumed error during the short grace period.
Unknown, expired, mixed-turn, incomplete, duplicate, and mismatched-kind
results are rejected without echoing call IDs or result content. Expired
records are removed from capacity accounting immediately; bounded call-ID
tombstones keep a recently expired submission distinguishable from an unknown
call before eventually becoming `continuation_unknown`. The default
store bounds are a five-minute pending TTL, a ten-minute consuming lease
deadline, 128 records, 16 MiB per record, 128 MiB aggregate, and a 30-second
consumed grace period. Pending state uses only the pending TTL; once `Begin`
reserves a result set, the active lease uses its own finite deadline so a
long-running upstream request can commit after the pending TTL without
retaining abandoned state forever. Cleanup, commit, and abort are serialized
by the store mutex; an active lease that reaches its deadline becomes
expired and cannot commit. All limits, lease durations, and the cleanup
interval are configurable with `ContinuationStoreConfig`. `Close` stops
the cleanup goroutine and releases retained state. Because v0.1 is
process-local, a gateway restart loses pending state and the next result must
be retried from a fresh provider turn; the gateway reports this as a
recoverable `continuation_unknown` error.

## HTTP ownership and errors

Construct a client with `NewClient(ClientConfig{...})`, injecting an
`HTTPDoer` and base URL in tests or future composition. The client creates a
new request and forwards only its own authorization, content type, accept, and
safe gateway user-agent headers. It does not copy inbound headers or mutate
`http.DefaultClient`.

Successful `Do` calls return a `Response` whose body is still open. The caller
must consume and close it, either with `response.Body.Close()` or
`response.Close()`. All non-success paths close upstream bodies before
returning. Error bodies are read only up to the configured bound and never
stored in `ProviderError`; status category and a validated `Retry-After` value
remain available as safe metadata. No automatic retries are performed.

The default HTTP client never follows redirects. A redirect response is
classified and its body is closed without exposing its contents, so the bearer
credential cannot be replayed to either a same-host or cross-host target.

The default transport uses proxy environment support, bounded connection
phases, keep-alive pooling, and disabled automatic compression to avoid
buffering behavior surprises in SSE. It has no total `http.Client.Timeout`;
request context cancellation owns the lifetime of an active stream.
