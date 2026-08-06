# Architecture and contribution boundaries

The first release has one narrow translation path:

```text
┌──────────────┐  Responses JSON/SSE   ┌─────────────────────┐  Chat Completions SSE  ┌────────────────┐
│ Codex CLI    │ ────────────────────> │ OpenCode Gateway    │ ─────────────────────> │ OpenCode Go   │
│ user config  │ <──────────────────── │ decode/bridge/SSE   │ <───────────────────── │ deepseek-v4   │
└──────────────┘   Responses events   └──────────┬──────────┘   provider stream      └────────────────┘
                                                 │
                                      loopback health + bounded logs
```

The boundary packages are intentionally directional:

| Package or area | Responsibility |
| --- | --- |
| `cmd/opencode-gateway` | CLI dispatch, credential command surface, exit semantics, signal wiring, safe build metadata output |
| `internal/config` | Environment parsing, defaults, validation, and finite limits |
| `internal/credentials` | Keyring-first API credential storage with a permission-restricted fallback |
| `internal/codex` | Codex request schema, field policy, Responses event state, SSE output |
| `internal/bridge` | Provider-neutral validated request and event values |
| `internal/opencodego` | OpenCode Go Chat Completions request/client, SSE decoding, bounded continuation state |
| `internal/server` | HTTP ownership, health routes, request admission, cancellation, and error envelope |
| `internal/codexsetup` | Safe user-level Codex config/catalog editing and doctor diagnostics |
| `scripts` and `Makefile` | Offline smoke, reproducible release packaging, and CI entry points |

Codex credentials and raw request content do not cross into logs or fixtures.
The server never executes provider-supplied command text. Changes to a wire
contract must update the decoder, bridge mapping, response state machine,
fixtures, policy, and documentation together.

## Contribution workflow

1. Read the repository [agent guidance](../AGENTS.md), the relevant contract
   document, and the applicable issue before editing.
2. Derive behavior from the documented Codex/OpenCode Go domain. Use TDD when
   a focused regression test is practical: start with a failing test, make the
   smallest domain-correct change, then refactor.
3. Keep tests meaningful. Do not weaken assertions or alter production
   behavior only to make a suite green; a test fix must preserve the actual
   application contract and security boundaries.
4. Keep production code dependency-free unless the user explicitly approves a
   new dependency. Ask before adding one.
5. Run the normal validation matrix before handoff:

   ```bash
   go fmt ./...
   go vet ./...
   go test -count=1 ./...
   go test -race -count=1 ./...
   go build -trimpath ./cmd/opencode-gateway
   git diff --check
   ```

6. For contract changes, also run `make contract`, `make integration`, and
   `make fuzz-smoke FUZZTIME=1s`; review redacted fixtures and update the
   compatibility matrix.
7. Use `type(scope): imperative message` commits and reference the issue when
   the work is issue-scoped. Do not commit credentials or raw captures.

## Release checklist

The release workflow is intentionally plain Go, shell, and GitHub CLI:

- [ ] Update the release notes and tested Codex compatibility row.
- [ ] Run `make release-check`; the release workflow also verifies that
      formatting leaves the tagged checkout unchanged.
- [ ] Run `make package-self-test` and inspect `dist/SHA256SUMS`.
- [ ] Validate an isolated `version`, `doctor`, setup, and health workflow as
      documented in [release.md](release.md).
- [ ] Perform only the explicitly approved opt-in paid smoke scenarios; never
      imply that deterministic tests prove a paid provider path.
- [ ] Tag `vX.Y.Z` only after review. The tag workflow cross-compiles, checks
      archives/checksums, and publishes with `gh release create`.
- [ ] After publication, download one archive per OS family, verify its
      checksum, run `version`, and record any compatibility caveat.
