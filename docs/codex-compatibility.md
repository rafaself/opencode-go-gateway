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

`scripts/live-smoke.sh` is the only automated real-provider smoke currently
enabled. It is text-only, opt-in, uses a temporary Codex home and repository,
and keeps the OpenCode Go key in the gateway process environment. Its offline
event-shape checks are run by `scripts/live-smoke-test.sh` and do not make a
network request.

The function-tool, `apply_patch`, and continuation paths have deterministic
full-handler integration coverage in `internal/server` and real redacted
fixtures. They are not invoked automatically by CI or by a generic paid
script: an unattended tool request could change a user's workspace or retain
provider state unexpectedly. A maintainer may add a scenario-specific,
temporary-workspace smoke after recording its safety conditions and expected
tool-result lifecycle in this matrix. Never treat the text smoke as proof of
those other scenarios.

No GitHub Actions workflow in this repository sends a paid inference request
or receives an API credential. Failures in the real smoke retain diagnostics
only in its temporary directory, as documented in the README.
