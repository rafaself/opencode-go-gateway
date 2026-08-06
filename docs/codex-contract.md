# Codex Responses compatibility contract

Issue #2 establishes the wire contract before translation logic is implemented. This project implements the subset of Responses required by tested Codex CLI versions; it does not implement the complete OpenAI Responses platform.

## Capture set

The checked-in request fixtures were normalized from the installed Codex CLI version `0.146.0`. The capture server records the version from `User-Agent` unless `--codex-version` is supplied explicitly. IDs, timestamps, prompts, instructions, source text, paths, environment values, and credential headers are redacted or normalized before a fixture is written.

The current request shape observed from Codex 0.146.0 includes `model`, `instructions`, `input`, `tools`, `tool_choice`, `parallel_tool_calls`, `reasoning`, `stream`, `store`, `include`, `prompt_cache_key`, and `client_metadata`. The request fixtures also cover the function-call and tool-result item shapes that occur across a tool turn.

Observed input item types are `message`, `function_call`, `function_call_output`, `custom_tool_call`, and `custom_tool_call_output`. Observed request tool types are `function`, `namespace`, and `web_search`; the latter two are explicitly deferred in the field policy. The `apply_patch` custom call is represented by `custom_tool_call` response items and their follow-up result items.

## Run a local capture

The command is deliberately development-only and binds to a resolved loopback IP. It refuses wildcard or non-loopback listener addresses, limits request bodies, never logs the raw body, and writes fixture files with restrictive permissions.

```bash
make build
./bin/opencode-gateway dev capture-codex \
  --name simple \
  --output-dir /tmp/opencode-codex-capture
```

The command prints a URL like `http://127.0.0.1:NNNNN/v1`. A current Codex CLI can be directed to it without editing a persistent configuration file:

```bash
codex exec \
  --ignore-user-config \
  --ephemeral \
  --skip-git-repo-check \
  --model gpt-5.3-codex \
  --sandbox read-only \
  --color never \
  --json \
  -c 'model_provider="capture"' \
  -c 'model_providers.capture={name="Capture", base_url="http://127.0.0.1:NNNNN/v1", wire_api="responses"}' \
  'Reply with exactly one short sentence.'
```

Replace `NNNNN` with the port printed by the capture server. Custom provider configuration and `wire_api = "responses"` follow the [Codex advanced configuration reference](https://developers.openai.com/codex/config-file/config-advanced). The [Codex configuration reference](https://developers.openai.com/codex/config-reference) documents the provider `base_url` and wire API settings.

Use a temporary workspace for tool-call captures. The mock response for `--response function` invokes only a fixed, harmless `printf` command when Codex executes it. `--response parallel` emits two fixed function calls. `--response custom` emits the `apply_patch` custom-tool event shape; Codex CLI 0.146.0 with fallback metadata reports that the mock custom call is unsupported, but it still records the request and tool-result structure for the contract fixture.

## Fixture layout

Request fixtures live in `testdata/codex/requests/`. Each fixture contains:

- the exact endpoint and safe request headers;
- a structurally representative request body with sensitive scalar values redacted;
- sorted top-level field, input-item-type, and tool-type indexes;
- the Codex version used for the capture;
- the response mode and ordered event types used for the mock response.

Response fixtures live in `testdata/codex/responses/` as SSE streams. They are parsed by tests, and all event sequence numbers are normalized to a strictly increasing deterministic sequence. No checked-in SSE fixture contains a `[DONE]` marker.

The transient output directory `testdata/codex/captures/` is ignored by Git. Review generated files before moving any capture into the checked-in fixture directories; redaction is a safety boundary, not a substitute for review. Reusing a fixture name and output directory is safe: the server skips existing numbered files and allocates the next available sequence.

## Field policy

`testdata/codex/field-policy.json` is the machine-readable policy used by the regression test. A newly captured top-level request field must be classified before a fixture can be checked in. The classifications are:

| Field | Policy | Current observation |
| --- | --- | --- |
| `model` | translate | observed |
| `instructions` | translate | observed |
| `input` | translate | observed; message and tool-result items are covered |
| `tools` | translate | observed; function, namespace, and web-search declarations are covered |
| `tool_choice` | translate | observed |
| `parallel_tool_calls` | translate | observed |
| `reasoning` | translate | observed |
| `text` | translate | evaluated; not emitted by the current capture |
| `stream` | translate | observed and required for SSE |
| `stream_options` | accept as no-op | evaluated; not emitted by the current capture |
| `store` | accept as no-op | observed as `false` |
| `include` | translate | observed |
| `service_tier` | accept as no-op | evaluated; not emitted by the current capture |
| `prompt_cache_key` | accept as no-op | observed; value is redacted |
| `metadata` / `client_metadata` | accept as no-op | transport metadata observed and redacted |
| `previous_response_id` | translate | covered by the continuation fixture |
| `background` | defer | not part of the first vertical slice |
| unknown fields | reject | regression test fails until explicitly classified |

Input and tool item types are classified in the `item_types` and `tool_types` maps in `testdata/codex/field-policy.json`. Function calls and their outputs, including the custom `apply_patch` call shape, are translated; Codex namespaces and standalone web search are deferred until a later vertical slice.

The capture server records unknown fields so that they cannot disappear during investigation. The M2 decoder in `internal/codex` applies the same policy before creating `internal/bridge` values: unknown fields and deferred top-level fields fail with stable structured errors, while the observed deferred `namespace` and `web_search` declarations are represented as explicit `bridge.DeferredTool` values rather than untyped provider maps. Compatibility no-op fields are type-checked and then omitted from the bridge model. `stream` must be present and `true` because the initial bridge milestone does not implement non-streaming Responses.

## M2 request boundary

`internal/codex.NewDecoder` requires a positive maximum body size. `Decoder.Decode` accepts only `application/json` (including valid media-type parameters), bounds reads before JSON allocation, rejects invalid UTF-8, malformed or trailing JSON, and rejects duplicate object keys. It validates required model, message, tool-call, and correlation values; function tool names are unique; function parameters and JSON-schema text formats have an object-root schema boundary; and errors expose stable `code`, `param`, and `message` fields without logging the request body. Boundary messages are fixed and safe generic parameters are used for unknown fields, so request values, schema keys, and raw JSON are never echoed in client-facing errors.

The decoder returns the explicit unions in `internal/bridge`: ordered messages with `input_text` content, function/custom calls and outputs, function tools with immutable raw JSON Schema, explicit tool choice, generation options, and known deferred tool markers. The bridge package has no dependency on Responses or OpenAI-specific wire structs, leaving upstream translation and SSE handling to later milestones.

## Minimum SSE sequences

The current Codex CLI accepted the following text sequence from the capture server:

```text
response.created
response.in_progress
response.output_item.added
response.content_part.added
response.output_text.delta
response.output_text.done
response.content_part.done
response.output_item.done
response.completed
```

For function calls, the required shape is:

```text
response.created
response.in_progress
response.output_item.added
response.function_call_arguments.delta
response.function_call_arguments.done
response.output_item.done
response.completed
```

For Codex custom `apply_patch`, the observed item and event shape is:

```text
response.created
response.in_progress
response.output_item.added   # item.type = custom_tool_call
response.custom_tool_call_input.delta
response.custom_tool_call_input.done
response.output_item.done
response.completed
```

Terminal fixtures cover `response.completed`, `response.incomplete`, and `response.failed`. The public [Responses streaming event reference](https://developers.openai.com/api/reference/resources/responses/streaming-events) defines the event fields, including `sequence_number`, response/item IDs, `output_index`, and `content_index`.

The capture run also verifies these ordering rules for the tested CLI:

1. `sequence_number` is monotonically increasing across the whole stream.
2. IDs remain stable between an item’s `.added`, `.delta`, and `.done` events.
3. Output and content indexes are zero-based and stable for an item.
4. `response.in_progress` is accepted before output events.
5. A terminal response is accepted when the HTTP stream ends after `response.completed`; a `[DONE]` marker is not required.

Cancellation is represented by `cancellation-request.json`: the request was recorded, then the client connection ended before a terminal SSE event. The capture server finalizes the request fixture after streaming and records only event frames successfully written before the interruption. A compatibility implementation must treat an interrupted transport as incomplete rather than inventing a successful terminal response.

## Recapture after a Codex upgrade

1. Record the new version with `codex --version`.
2. Start the capture server with a temporary output directory and an explicit fixture name.
3. Run the simple prompt first, then the developer/instruction, workspace read, shell, function, parallel, custom tool, result, empty result, error, continuation, and interruption scenarios.
4. Inspect every generated file for prompt text, source code, paths, environment values, and credentials.
5. Move only reviewed redacted fixtures into `testdata/codex/requests/` and update `field-policy.json` for any new top-level field.
6. Update the version recorded in this document and run `go test ./...`.

The fixture tests intentionally fail if a request field is not classified or if an SSE fixture is not valid JSON-per-event with increasing sequence numbers.

The incremental provider decoder and per-request Responses session introduced
by M4 are documented in [Streaming boundaries](codex-streaming.md). They keep
provider chunks, bridge semantics, and Responses wire state in separate
packages. Issues #7 and #8 wire the text and standard function-tool vertical
slices through those boundaries; the manual smoke workflow is documented in the
[project README](../README.md#first-text-only-smoke-test).
