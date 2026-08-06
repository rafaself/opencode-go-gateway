# Codex compatibility and end-to-end verification

The gateway targets the Codex Responses wire contract recorded in
`testdata/codex`. Compatibility is checked at three layers:

1. request and response fixtures are parsed as executable contracts;
2. the complete gateway route is exercised with deterministic provider
   streams; and
3. an opt-in real Codex smoke run is performed only when a user supplies a
   valid OpenCode Go credential.

## Compatibility matrix

The matrix records the exact CLI version and operating system used for a
capture. A fixture update is not complete until the corresponding row is
updated and the redacted output is reviewed.

| Codex CLI | OS/architecture | Config mode | Coverage | Status |
| --- | --- | --- | --- | --- |
| 0.146.0 | Linux, amd64 | user-level `config.toml`, `responses` provider | checked-in request/response fixtures, setup/doctor tests, offline smoke assertions | baseline recorded |
| 0.146.0 | macOS, native | user-level `config.toml`, `responses` provider | run the recapture procedure below | pending local verification |
| 0.146.0 | Windows, native | user-level `config.toml`, `responses` provider | run the recapture procedure below | pending local verification |
| newer or older version | each supported OS | user-level `config.toml`, `responses` provider | recapture and compare the contract | not assumed compatible |

The repository does not claim cross-platform or paid-provider compatibility
from a Linux-only test run. Record a new version instead of silently changing
the baseline when Codex changes its request fields, event ordering, or config
schema.

## Safe recapture procedure

Run this procedure from a temporary directory. Do not point it at a personal
Codex home and do not put `OPENCODE_GO_API_KEY` in a file.

```bash
set -eu
tmp_dir=$(mktemp -d)
codex_home="$tmp_dir/codex-home"
capture_dir="$tmp_dir/captures"
mkdir -p "$codex_home" "$capture_dir"
export CODEX_HOME="$codex_home"
codex --version

make build
./bin/opencode-gateway setup codex --codex-home "$codex_home" --dry-run
./bin/opencode-gateway setup codex --codex-home "$codex_home"
./bin/opencode-gateway dev capture-codex --once \
  --name codex-version-check \
  --output-dir "$capture_dir"
```

The capture server is loopback-only. Configure the temporary Codex profile to
use its printed `base_url`, run one harmless request, stop the server, and
inspect the redacted JSON before copying anything into `testdata/codex`.
Record:

- the exact output of `codex --version`;
- operating system and architecture;
- the provider `base_url` and `wire_api` (never the API key);
- request top-level fields, input item types, and tool types;
- response event types, sequence ordering, terminal event, and whether a
  `[DONE]` marker was present; and
- the result of `make test`, `make race`, and `make fuzz-smoke`.

For a checked-in fixture, run the contract tests and review the redacted
diff:

```bash
make contract
git diff --check
rg -n --hidden --glob '!*.lock' \
  'Bearer |sk-|OPENAI_API_KEY|OPENCODE_GO_API_KEY|/home/|/Users/|C:\\\\Users\\\\' \
  "$capture_dir"
```

Do not copy a capture that contains prompts, source code, filesystem paths,
environment values, credentials, or client identifiers. The fixture
validation suite must fail if a newly observed field or type is not classified
in `testdata/codex/field-policy.json`.

## Paid smoke policy

The real-provider scenarios are deliberately opt-in and are never run by CI,
the release workflow, or `make release-check`. They require explicit
authorization and can incur one or more billable model/tool turns per selected
scenario:

```bash
make build
RUN_LIVE_SCENARIOS=1 OPENCODE_GO_API_KEY='your-key' \
  ./scripts/live-scenarios.sh --all
```

Use scenario names to limit cost (`text`, `inspect`, `shell`, `function`,
`apply-patch`, `parallel`, and `cancel`). The suite covers:

| Scenario | Required live evidence |
| --- | --- |
| `text` | completed text turn and assistant-message event |
| `inspect` | command execution against the generated repository, with no edits |
| `shell` | one fixed harmless shell command |
| `function` | one standard function/tool event and completed turn |
| `apply-patch` | an apply-patch/file-change event and exact generated-file edit |
| `parallel` | two tool starts before the first tool completion |
| `cancel` | a started long-running streamed response, client cancellation, and healthy gateway afterward |

The script starts the built binary on an ephemeral loopback port and uses a
temporary, owner-only Codex home and Git repository. It runs Codex with
`--ignore-user-config`, `--ephemeral`, and explicit provider overrides. The
OpenCode Go key is assigned only to the gateway child and is explicitly unset
for Codex. Codex runs in the generated repository, never in the project
worktree. Prompts and tool text are fixed by the script; the script never
executes arbitrary incoming tool text.

Codex stdout is read through a FIFO and reduced immediately to structural
JSONL containing only allowlisted event types, item types, phases, statuses,
and booleans. Text, arguments, outputs, IDs, paths, usage, and raw Codex
stderr are not retained. Bounded timeouts, low-rate one-second checks, cleanup,
health checks, and leak scans cover the safety boundary. A model or provider
that does not produce the required behavior fails the named scenario; it is
never reported as a pass. Failed runs retain only private structural events
and safe gateway diagnostics, while the generated home, repository, and raw
Codex stderr are discarded.

The credential-free validation path checks shell syntax, scenario names,
opt-in gating, process isolation markers, and structural redaction:

```bash
make live-scenarios-test
./scripts/live-scenarios.sh --validate --all
```

The existing `RUN_LIVE_SMOKE=1 ./scripts/live-smoke.sh` command remains the
small text-only incremental-output smoke. Its offline tests are
`scripts/live-smoke-test.sh`; it is not evidence for the tool, patch,
parallel, or cancellation scenarios. The compatibility matrix must record the
Codex version, OS, architecture, selected scenarios, and whether each paid
run actually completed.
