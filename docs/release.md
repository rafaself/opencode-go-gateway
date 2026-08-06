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
Windows amd64, then verifies every archive contains the correctly named binary,
`LICENSE`, and `README.txt`, and verifies `SHA256SUMS`.

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
`.zip` and contains `opencode-gateway.exe`. Each archive has one predictable
top-level directory and includes only the binary, MIT `LICENSE`, and minimal
`README.txt`. `SHA256SUMS` covers the archives only.

## RC validation procedure

This procedure uses an isolated Codex home and a fixed local port. It does not
write a provider key to disk. The doctor command performs a network `/models`
check only when `OPENCODE_GO_API_KEY` is present, so keep the credential in the
shell environment and do not paste it into logs.

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

Paid inference is never part of CI or the release workflow. The opt-in text
smoke is:

```bash
RUN_LIVE_SMOKE=1 ./scripts/live-smoke.sh
```

It requires a user-supplied `OPENCODE_GO_API_KEY`, Codex CLI, and network
access, and uses a temporary Codex home/repository. The text smoke does not
prove function-tool, `apply_patch`, or continuation behavior. Those paths have
deterministic contract/integration coverage, but an operator must explicitly
approve and run a scenario-specific paid smoke before claiming that gate. A
release must never claim those unavailable live scenarios were run.

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
