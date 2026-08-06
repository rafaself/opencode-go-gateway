# Release and RC validation

Release tooling is deliberately limited to Go, POSIX shell, GNU/BSD archive
utilities, and the GitHub CLI already available on the GitHub Actions runner.
There is no cloud deployment or package-manager automation.

## Local release checks

From a clean checkout, run the full deterministic gate:

```bash
make release-check FUZZTIME=1s
make package-self-test VERSION=v0.1.0 OUTPUT_DIR=dist
```

`release-check` runs formatting, vet, unit/contract/integration tests, the race
suite, a trimmed build, bounded fuzz smoke, and `git diff --check`. The release
workflow additionally requires that formatting leaves the tagged checkout
unchanged.
`package-self-test` cross-compiles Linux amd64/arm64, macOS amd64/arm64, and
Windows amd64, then verifies every archive contains the correctly named
`opencode-gateway` and `ocgtw` binaries, `LICENSE`, and `README.txt`, and
verifies `SHA256SUMS`.

The repository installer is `scripts/install.sh`. It builds the current
checkout when Go is available; otherwise it can download a published Unix
archive and its `SHA256SUMS` manifest over HTTPS. It verifies the exact
checksum before extracting, rejects unexpected archive paths, stages both
binaries before replacement, and never requires root. A release must publish
the expected archive names and `SHA256SUMS` for the no-Go path to work:

```bash
./scripts/install.sh --release --version v0.1.0 --prefix "$HOME/.local"
```

For a local artifact, pass both files explicitly when they are not siblings:

```bash
./scripts/install.sh \
  --archive /path/to/opencode-gateway_v0.1.0_linux_amd64.tar.gz \
  --checksums /path/to/SHA256SUMS \
  --prefix "$HOME/.local"
```

The installer has no checksum bypass option. Its Bash release path supports
Linux and macOS amd64/arm64; Windows users should download and verify the zip
archive manually.

The package script uses the tagged commit time as its default
`SOURCE_DATE_EPOCH`. To reproduce an artifact exactly, provide the same
version, commit, build date, source epoch, Go toolchain, and source tree:

```bash
SOURCE_DATE_EPOCH=1762473600 \
make package-self-test \
  VERSION=v0.1.0 \
  COMMIT=0123456789ab \
  OUTPUT_DIR=/tmp/opencode-gateway-dist
```

Archives are named `opencode-gateway_VERSION_GOOS_GOARCH.tar.gz`; Windows uses
`.zip` and contains `opencode-gateway.exe` and `ocgtw.exe`. Each archive has
one predictable top-level directory and includes only the two binaries, MIT
`LICENSE`, and minimal `README.txt`. `SHA256SUMS` covers the archives only.

## RC validation procedure

This procedure uses an isolated Codex home and a fixed local port. The
credential store is separate from the Codex home and is never included in
release artifacts. The doctor command performs a network `/models` check only
when a credential is present, so use an isolated environment variable or
`ocgtw config status` and do not paste the credential into logs.

```bash
set -eu
make build
./bin/opencode-gateway version
./bin/opencode-gateway help

rc_dir="$(mktemp -d)"
codex_home="$rc_dir/codex-home"
dist_dir="$rc_dir/dist"
mkdir -p "$codex_home"

./bin/opencode-gateway setup codex --codex-home "$codex_home"
./bin/opencode-gateway setup codex --codex-home "$codex_home" --dry-run

export OPENCODE_GO_API_KEY="${OPENCODE_GO_API_KEY:?set this only in the process environment}"
export OPENCODE_GATEWAY_PORT=18787
./bin/opencode-gateway run >"$rc_dir/gateway.log" 2>&1 &
gateway_pid=$!
trap 'kill "$gateway_pid" 2>/dev/null || true; rm -rf "$rc_dir"' EXIT

curl --fail --silent http://127.0.0.1:18787/health/live
curl --fail --silent http://127.0.0.1:18787/health/ready
./bin/opencode-gateway doctor --codex-home "$codex_home"

SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)" \
  make package-self-test VERSION=v0.1.0 COMMIT="$(git rev-parse --short HEAD)" OUTPUT_DIR="$dist_dir"
(cd "$dist_dir" && sha256sum -c SHA256SUMS)
```

On macOS, use `shasum -a 256` when `sha256sum` is unavailable. On Windows,
perform the same isolated setup and health checks with PowerShell and verify
the published checksum using `Get-FileHash -Algorithm SHA256`. Record the
actual OS/architecture and Codex version; the repository baseline is Codex
CLI `0.146.0` on Go 1.22+.

The RC gate must also confirm that `doctor` returns exit status `0` for a
healthy configured gateway and a non-zero status when the key, catalog, Codex
config, or gateway is missing. It must confirm that `version` and `help` write
safe output and that logs contain neither the key nor request content.

## Paid smoke policy

Paid inference is never part of CI, the release workflow, or
`make release-check`. The existing opt-in text smoke is:

```bash
RUN_LIVE_SMOKE=1 ./scripts/live-smoke.sh
```

The required issue #13 live-validation suite is:

```bash
RUN_LIVE_SCENARIOS=1 OPENCODE_GO_API_KEY='your-key' \
  ./scripts/live-scenarios.sh --all
```

It makes one isolated paid request per selected scenario: text, temporary
repository inspection, a harmless shell command, one standard function/tool
turn, `apply_patch` editing, a parallel-tool attempt, and cancellation of a
long-running request. The full suite can therefore incur seven billable
requests plus provider-side tool/continuation usage. Select scenario names to
limit cost. The suite requires a built binary, Codex CLI, network access, and
explicit operator authorization; `./scripts/live-scenarios.sh --validate --all`
and `make live-scenarios-test` are credential-free checks.

The suite creates an owner-only temporary Codex home and Git repository,
starts the gateway on an ephemeral loopback port, keeps the key only in the
gateway child environment, and unsets it for Codex. It never uses the project
worktree or executes arbitrary incoming tool text. It retains only structural
JSONL and safe diagnostics on failure, cleans up on success, and fails clearly
when the live model/provider does not produce a required scenario. It is not a
CI or release gate. Record the exact Codex version, OS, architecture, selected
scenarios, and actual result before treating the paid gate as complete.

## Publishing

Do not publish from a dirty tree. After the RC procedure, create and push a
`vX.Y.Z` tag according to the repository's Git policy. The tag workflow runs
`make release-check`, calls `scripts/package-release.sh --self-test`, and uses
`gh release create` to attach archives and `SHA256SUMS`. It uses the matching
`docs/releases/vX.Y.Z.md` file when present. Do not create a release manually
before the workflow completes.

After publication, download at least one Unix archive and the Windows archive,
verify their checksums, run `opencode-gateway version`, and retain the release
asset names and tested Codex/OS versions in the release record.

## Rollback and uninstall

For a Codex configuration change, use the backup path printed by setup:

```bash
./bin/opencode-gateway setup codex --codex-home "$codex_home" --restore \
  "/absolute/path/to/backup-opencode-gateway-..."
```

To uninstall the gateway, stop the local process and remove the extracted
binary/archive. Remove the managed `opencode-gateway` provider and catalog
from the Codex home only after taking a backup, or use the setup backup to
restore the exact pre-setup files. The gateway does not install a service or
write outside the selected Codex home and release directory.
