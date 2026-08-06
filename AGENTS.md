# Agent guidance

## Scope

This file applies to the whole repository. Read it before making changes. If a more specific `AGENTS.md` is added later, its instructions apply to files beneath its directory in addition to this file.

## Project context

- This is a Go 1.22+ project and currently uses only the Go standard library.
- The current vertical slice captures and documents the Codex CLI Responses contract. It is not a complete OpenAI Responses implementation.
- The capture command is development-only: `./bin/opencode-gateway dev capture-codex` after `make build`.
- The contract, field policy, and checked-in fixtures are maintained together under `docs/` and `testdata/codex/`.

## Non-negotiable safety rules

- Ask for confirmation before adding any new production dependency. Prefer the standard library when it is sufficient.
- Do not log, commit, or expose raw prompts, instructions, source code, filesystem paths, environment values, credentials, authorization headers, or client metadata.
- Keep the capture server loopback-only. Do not add wildcard/public binding or arbitrary command execution to the capture path.
- Treat redaction as a security boundary, not as a reason to skip review. Add or update tests whenever redaction or fixture serialization changes.
- Do not introduce changes that create security vulnerabilities. If a necessary change has material security implications, stop and warn the user before proceeding.
- Do not retain dead code, obsolete paths, or compatibility shims solely for legacy behavior. Remove superseded code when changing a contract unless an explicit requirement says otherwise.

## Development workflow

- Inspect the current worktree and preserve unrelated user changes. Never use destructive history or filesystem commands such as `git reset --hard` or broad recursive deletion.
- Use `apply_patch` for hand-edited files. Run `gofmt` on changed Go files.
- Keep changes focused and update implementation, tests, documentation, and fixtures together when they describe the same behavior.
- Use TDD whenever it is practical: start with a focused failing test, implement the smallest correct change, then refactor and keep the test as regression coverage. Do not force TDD for documentation-only, configuration-only, or otherwise trivial changes where it adds no value.
- Treat tests as executable expressions of the application domain, not merely as a gate to turn green. When changing tests or the test suite, derive expected behavior primarily from project documentation, the contract, and domain semantics; when documentation is incomplete or ambiguous, use sound judgment to choose and document the best domain-aligned approach.
- Never weaken assertions, remove meaningful coverage, add tautological tests, or distort production behavior solely to satisfy the current suite. A correction is complete only when both code and tests preserve the project's quality criteria and the real behavior of the application domain.
- Prefer deterministic outputs: normalize IDs and timestamps in fixtures, avoid clock- or network-dependent tests, and use temporary directories for generated captures.
- Keep production code dependency-free unless the user explicitly approves a new dependency.

## Validation

Before handing off a Go change, run the checks relevant to its scope; for normal implementation changes run all of these:

```bash
go fmt ./...
go vet ./...
go test -count=1 ./...
go test -race ./...
go build -trimpath ./cmd/opencode-gateway
git diff --check
```

For contract or capture changes, additionally verify that:

- every checked-in request fixture is valid JSON and classified in `testdata/codex/field-policy.json`;
- every response fixture is valid SSE with valid JSON events, increasing `sequence_number` values, and an explicit terminal event;
- no fixture contains secrets, prompt text, source code, absolute user paths, or environment values;
- the exact Codex CLI version used for a capture is recorded and recapture instructions remain accurate.

## Contract and capture rules

- A newly observed request field or item/tool type must be explicitly classified as `translate`, `accept as no-op`, `reject`, or `defer`; never silently ignore it.
- Keep the capture endpoint limited to `POST /v1/responses` unless the contract task explicitly expands scope.
- Do not check generated captures into Git without reviewing the redacted output. Use a temporary output directory while capturing; the repository capture directory is disposable.
- Mock response modes must use fixed, harmless tool calls. Never execute command text supplied by an incoming request.
- When changing SSE behavior, preserve and test event ordering, stable IDs, indexes, terminal status, and behavior without a `[DONE]` marker.

## Git and GitHub

- Do not commit, push, create pull requests, modify issues, or send external messages unless the user explicitly requests that action.
- Use the commit format `type(scope): message`, with a concise imperative message. Typical types include `feat`, `fix`, `test`, `docs`, `refactor`, `build`, and `chore`; use the smallest accurate scope, such as `capture`, `contract`, or `agent`.
- When a task is explicitly tied to an issue, reference it after the subject, for example `feat(capture): add Codex contract fixtures (#2)`.
- Before any push or issue mutation, verify the branch, staged diff, commit scope, and remote target. Never force-push or rewrite history unless explicitly authorized.
- Do not create a pull request when the user asks for direct branch publication.
