# Streaming boundaries

Issue #6 keeps provider streaming and Codex Responses serialization in separate
packages. The provider adapter owns only Chat Completions wire details; the
Responses session owns IDs, indexes, sequence numbers, event names, and
terminal state.

## Provider-to-bridge API

`internal/opencodego.NewBridgeStreamDecoder(reader, options)` returns a
per-response decoder. The HTTP adapter passes the request's declared
function names in `BridgeStreamDecoderOptions.AllowedToolNames`; that
per-request allowlist is copied and checked against the complete provider
name at the terminal boundary. Call `Next()` until it returns `io.EOF`:

```go
decoder := opencodego.NewBridgeStreamDecoder(response.Body, opencodego.BridgeStreamDecoderOptions{
    SSE:              opencodego.SSEDecoderOptions{},
    AllowedToolNames: []string{"lookup"},
})
for {
    event, err := decoder.Next()
    if errors.Is(err, io.EOF) {
        break
    }
    if err != nil {
        // A transport/decoder error can be mapped to bridge.Failed by the
        // request orchestrator.
        return err
    }
    if err := session.Handle(event); err != nil {
        return err
    }
}
```

`Next` emits `bridge.ResponseStarted`, text/reasoning deltas, indexed tool
call lifecycle events, usage updates, and one terminal semantic event. Tool
call semantic events are buffered until the provider name is final, because
Chat Completions has no earlier name-finalization marker: an allowlisted
`lookup` fragment may still be extended to undeclared `lookupEvil`. This
preserves the security boundary at the cost of emitting valid tool calls at
the terminal boundary rather than speculatively. The provider `[DONE]` marker
is consumed internally and is never a bridge event. After the first marker has
produced a semantic terminal event, the bridge uses a fail-closed no-read-ahead
policy: `Next` returns `io.EOF` without inspecting later provider bytes. This
keeps the HTTP boundary from blocking on an irrelevant post-terminal event. A
caller that owns the lower-level provider decoder can continue draining
`ChatCompletionStreamDecoder` and receive `ErrDuplicateStreamTerminal` for a
later terminal marker; the gateway server intentionally does not perform that
read-ahead.
Provider response IDs, creation time, model, choice indexes, tool indexes,
fragmented IDs/names/arguments, finish reasons, usage, and typed provider
errors remain available at the provider boundary without importing Codex wire
types. The bridge rejects a truncated SSE event, missing/negative tool index,
and incoherent deltas after a choice has reported a finish reason. Provider
reconstruction is bounded by `SSEDecoderOptions.MaxAggregateBytes`. Argument
fragments for both ordinary functions and the synthetic `apply_patch` function
are charged when retained, before state mutation; their later semantic
lifecycle events do not charge the same argument bytes twice.

Provider reconstruction also enforces bounded choice/tool-call counts,
tool indexes, fragmented tool names, and private provider call IDs before
retaining provider state. It also enforces `MaxToolCallArgumentBytes`. A missing
provider tool-call ID receives the deterministic `call_<choice>_<tool>`
mapping. The downstream `call_id` is fixed before its output item is emitted;
later provider ID fragments are retained privately through `ProviderCallID`
for a future provider-specific continuation adapter and never rewrite
Responses event identity. Repeated tool indexes within one provider chunk and
duplicate completed call IDs are treated as stream inconsistencies, while the
same index remains valid across argument fragments. A `tool_calls` finish
reason may precede trailing fragments for already-started calls, but trailing
text, reasoning, new indexes, or contradictory terminal reasons are rejected.

Transport read failures retain their typed cancellation, timeout, and network
interruption causes; malformed SSE remains a provider protocol error. For
tests or specialized adapters, `NewChatCompletionStreamDecoder` exposes
typed `ChatCompletionChunk` and `ProviderStreamError` values directly. The
underlying `NewSSEDecoder` accepts explicit line, event, buffered-byte, and
reader-buffer limits and is safe under arbitrary reader chunk boundaries.

## Bridge-to-Responses API

Create one `internal/codex.StreamSession` per HTTP request. The session can be
started explicitly or started automatically by the first
`bridge.ResponseStarted`/semantic event:

```go
session, err := codex.NewStreamSession(w, codex.StreamSessionOptions{
    ResponseID: responseID,
    CreatedAt:  time.Now(),
    Model:      model,
})
if err != nil {
    return err
}

for {
    event, err := decoder.Next()
    if errors.Is(err, io.EOF) {
        break
    }
    if err != nil {
        _ = session.Fail("upstream_stream_error", "The upstream stream failed.")
        return err
    }
    if err := session.Handle(event); err != nil {
        return err
    }
}
```

The session sets `text/event-stream`, no-cache, keep-alive, and buffering
headers, and flushes every meaningful wire event. It emits one
`response.created`, one `response.in_progress`, ordered item/content events,
and exactly one of `response.completed`, `response.incomplete`, or
`response.failed`. A write or flush failure is returned as `ErrStreamWrite`;
the session records that failure and never attempts a second response.

`StreamSessionOptions.CustomTools` registers typed custom-tool event hooks by
bridge tool kind. The default `bridge.ToolCustom` hook produces the captured
`custom_tool_call` event names. Issue #9 supplies the provider-side
request-scoped registry: fragmented `__ocg_apply_patch` function arguments are
unwrapped only after the strict object is complete, and the bridge emits the
public `apply_patch` custom events. The synthetic provider name is never
passed to the Responses session.

The session generates and owns the public Responses response ID. A provider
`ResponseStarted.ID` is private correlation metadata and is never copied into
the downstream response. `StreamSessionOptions.Clock` controls terminal
timestamps in deterministic tests, and `MaxAggregateBytes` bounds retained
text, reasoning, tool IDs/names/arguments, and session state. Terminal
successful responses record their terminal timestamp separately from
`created_at`; incomplete and failed responses serialize `completed_at` as
`null` because they did not complete successfully.

The session mutex serializes concurrent `Handle` calls. If a downstream write
or flush fails, `Handle`/`Start` returns `ErrStreamWrite`, `WriteFailure()`
exposes the stable error, and `Done()` is closed exactly once. Issue #7 owns
the upstream response body and request context: it must observe `Done()` and
cancel/close that upstream resource. The session deliberately does not own or
close the provider body.

## Issue #7 request orchestration

`internal/server` composes these boundaries for `POST /v1/responses`:

1. `codex.Decoder.DecodeRequest` validates the request media type, body size,
   UTF-8, duplicate keys, trailing values, and field policy.
2. The server creates a request-scoped tool registry, accepts standard
   function tools plus the exact captured `mcp`/`web_search` metadata and
   `apply_patch` custom call/result shapes, and rejects unknown custom/deferred
   tools, function tool-call/result inputs, structured output formats, and
   continuation state before calling the provider.
3. The injected `server.UpstreamClient` returns only a provider-neutral status,
   headers, and body. Status and `text/event-stream` are validated before the
   downstream session commits headers.
4. A child context is passed to the upstream client. A downstream write/flush
   failure or inbound cancellation cancels that context and closes the body;
   the watcher is joined before the handler returns.
5. The bridge decoder and Responses session process one semantic event at a
   time. Provider errors, malformed JSON, truncated streams, and malformed
   custom-tool wrappers become one terminal `response.failed` event when
   downstream delivery is still possible; `finish_reason=length` becomes
   `response.incomplete`.

The request context remains the authority for client and shutdown cancellation.
The provider child context can additionally be canceled by the stream-idle
watchdog or a downstream write failure. That separation lets the gateway stop
the provider body promptly while still emitting one safe timeout
`response.failed` event when the client is able to receive it. A canceled
client receives no fallback JSON and the pending continuation lease is aborted
so the tool turn can be retried.

The application constructs the OpenCode Go client from `OPENCODE_GO_API_KEY`
and `OPENCODE_GO_BASE_URL`. The key is never sent to Codex or included in
logs, errors, or Responses bytes. See the [provider smoke test](../README.md#first-text-only-smoke-test)
for the manual verification record.
