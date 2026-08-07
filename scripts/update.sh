#!/usr/bin/env bash
set -euo pipefail

# Update an existing opencode-gateway install to a newer version. Only an
# install recorded by scripts/install.sh can be updated: the install manifest
# is required, and installed files that no longer match their recorded hashes
# block the update unless --force is used. A downgrade is refused unless
# --force. When the requested version is already installed, the update is a
# no-op. The actual replacement is delegated to scripts/install.sh, which
# verifies checksums, stages both binaries, and rewrites the manifest.
umask 077

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
REPOSITORY="rafaself/opencode-go-gateway"
GO_BIN="${GO:-go}"
PREFIX_VALUE="${PREFIX:-}"
INSTALL_DIR_VALUE="${INSTALL_DIR:-}"
VERSION_VALUE=""
ARCHIVE_VALUE=""
CHECKSUMS_VALUE=""
SOURCE_MODE=0
RELEASE_MODE=0
DRY_RUN=0
FORCE=0
MANIFEST_NAME=".opencode-gateway.manifest"

die() {
	echo "opencode-gateway update: $*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/update.sh [options]

Update an existing opencode-gateway install to a newer version. An install
manifest (written by scripts/install.sh) must be present, and installed files
must still match their recorded hashes; otherwise the update refuses to
proceed unless --force is used. Refuses to downgrade unless --force. When the
requested version is already installed, the update is a no-op.

By default, update to the latest published release, or rebuild from this
checkout when Go is available, like scripts/install.sh.

Options:
  --prefix DIR             Installation prefix (default: $HOME/.local)
  --install-dir DIR        Exact directory containing the binaries
  --version VERSION        Target version, for example v0.2.0
  --repository OWNER/REPO  GitHub repository (default: rafaself/opencode-go-gateway)
  --source                 Require a local source build
  --release                Require a published GitHub release
  --archive FILE           Install a local release archive
  --checksums FILE         Checksum manifest for --archive (default: sibling SHA256SUMS)
  --force                  Proceed despite a downgrade or modified files
  --dry-run                Show the planned update without changing anything
  -h, --help               Show this help
EOF
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

sha256_file() {
	local path="$1"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$path" | awk '{print $1}'
		return
	fi
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$path" | awk '{print $1}'
		return
	fi
	die "required command not found: sha256sum or shasum"
}

prepare_destination() {
	local home_value
	if [ -z "$PREFIX_VALUE" ]; then
		home_value="$(printenv HOME 2>/dev/null || true)"
		[ -n "$home_value" ] || die "use --prefix because HOME is not set"
		PREFIX_VALUE="$home_value/.local"
	fi
	if [ -z "$INSTALL_DIR_VALUE" ]; then
		INSTALL_DIR_VALUE="$PREFIX_VALUE/bin"
	fi
	[[ "$INSTALL_DIR_VALUE" == /* ]] || die "installation directory must be an absolute path"
}

# read_manifest_version prints the version recorded in the install manifest
# and returns 0, or returns 1 when the manifest has no version line.
read_manifest_version() {
	local manifest="$1" entry
	[ -f "$manifest" ] && [ ! -L "$manifest" ] || return 1
	while IFS= read -r entry; do
		[ -n "$entry" ] && [ "${entry#\#}" = "$entry" ] || continue
		case "$entry" in
			version=*)
				printf '%s\n' "${entry#version=}"
				return 0
				;;
		esac
	done < "$manifest"
	return 1
}

checkout_version() {
	git -C "$ROOT_DIR" describe --tags --always --dirty 2>/dev/null || printf 'dev'
}

# version_number turns a dotted numeric version (optionally v-prefixed) into a
# comparable string, or returns 1 when the version carries a suffix such as a
# build or prerelease tag. Versions that cannot be compared are never used for
# downgrade decisions.
version_number() {
	local v="$1"
	v="${v#v}"
	case "$v" in
		''|*[!0-9.]*|.*|*. ) return 1 ;;
	esac
	printf '%s' "$v"
}

# version_less returns 0 when a < b for two dotted numeric versions.
version_less() {
	local a="$1" b="$2" i na nb
	local -a ia ib
	IFS=. read -ra ia <<< "$a"
	IFS=. read -ra ib <<< "$b"
	for ((i = 0; i < ${#ia[@]} || i < ${#ib[@]}; i++)); do
		na="${ia[$i]:-0}"
		nb="${ib[$i]:-0}"
		if ((10#$na < 10#$nb)); then
			return 0
		fi
		if ((10#$na > 10#$nb)); then
			return 1
		fi
	done
	return 1
}

# resolve_latest_version prints the latest published release version, or
# returns 1 when it cannot be determined. The delegated installer reports the
# authoritative error when no release exists.
resolve_latest_version() {
	local latest_url
	require_command curl
	latest_url="$(curl --fail --silent --show-error --location \
		--proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 120 \
		--output /dev/null --write-out '%{url_effective}' \
		"https://github.com/$REPOSITORY/releases/latest")" || return 1
	case "$latest_url" in
		"https://github.com/$REPOSITORY/releases/tag/"*)
			printf '%s\n' "${latest_url##*/}"
			return 0
			;;
		*) return 1 ;;
	esac
}

# verify_manifest_files warns (but does not fail) when installed files no
# longer match the manifest. Used only on the no-op path, where nothing will
# be written.
verify_manifest_files() {
	local manifest="$1" entry recorded_name recorded_hash actual path
	while IFS= read -r entry; do
		[ -n "$entry" ] && [ "${entry#\#}" = "$entry" ] || continue
		case "$entry" in
			file\ *) ;;
			*) continue ;;
		esac
		set -- $entry
		recorded_name="$3"
		recorded_hash="$4"
		[ -n "$recorded_name" ] && [ -n "$recorded_hash" ] || continue
		path="$INSTALL_DIR_VALUE/$recorded_name"
		if [ -e "$path" ] || [ -L "$path" ]; then
			if [ ! -f "$path" ] || [ -L "$path" ]; then
				echo "opencode-gateway update: warning: installed $recorded_name is not a regular file" >&2
				continue
			fi
			actual="$(sha256_file "$path")"
			[ "$actual" = "$recorded_hash" ] ||
				echo "opencode-gateway update: warning: installed $recorded_name differs from the install manifest" >&2
		fi
	done < "$manifest"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--prefix)
			[ "$#" -ge 2 ] || die "--prefix requires a value"
			PREFIX_VALUE="$2"
			shift 2
			;;
		--install-dir)
			[ "$#" -ge 2 ] || die "--install-dir requires a value"
			INSTALL_DIR_VALUE="$2"
			shift 2
			;;
		--version)
			[ "$#" -ge 2 ] || die "--version requires a value"
			VERSION_VALUE="$2"
			shift 2
			;;
		--repository)
			[ "$#" -ge 2 ] || die "--repository requires a value"
			REPOSITORY="$2"
			shift 2
			;;
		--source)
			SOURCE_MODE=1
			shift
			;;
		--release)
			RELEASE_MODE=1
			shift
			;;
		--archive)
			[ "$#" -ge 2 ] || die "--archive requires a value"
			ARCHIVE_VALUE="$2"
			shift 2
			;;
		--checksums)
			[ "$#" -ge 2 ] || die "--checksums requires a value"
			CHECKSUMS_VALUE="$2"
			shift 2
			;;
		--force)
			FORCE=1
			shift
			;;
		--dry-run)
			DRY_RUN=1
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			die "unknown option: $1"
			;;
	esac
done

prepare_destination
manifest="$INSTALL_DIR_VALUE/$MANIFEST_NAME"
INSTALLED_VERSION=""
if [ -f "$manifest" ] && [ ! -L "$manifest" ]; then
	INSTALLED_VERSION="$(read_manifest_version "$manifest")" ||
		die "install manifest is missing a version: $manifest"
else
	((FORCE)) ||
		die "no install manifest found at $manifest; run scripts/install.sh to install, or use --force"
fi

TARGET_VERSION=""
DELEGATE_ARGS=()
if ((SOURCE_MODE)); then
	DELEGATE_ARGS+=(--source)
fi
if ((RELEASE_MODE)); then
	DELEGATE_ARGS+=(--release)
fi
DELEGATE_ARGS+=(--repository "$REPOSITORY")
if [ -n "$VERSION_VALUE" ]; then
	TARGET_VERSION="$VERSION_VALUE"
	DELEGATE_ARGS+=(--version "$VERSION_VALUE")
elif [ -n "$ARCHIVE_VALUE" ]; then
	DELEGATE_ARGS+=(--archive "$ARCHIVE_VALUE")
	if [ -n "$CHECKSUMS_VALUE" ]; then
		DELEGATE_ARGS+=(--checksums "$CHECKSUMS_VALUE")
	fi
	case "$(basename -- "$ARCHIVE_VALUE")" in
		opencode-gateway_v*)
			archive_version="$(basename -- "$ARCHIVE_VALUE")"
			archive_version="${archive_version#opencode-gateway_v}"
			archive_version="${archive_version%.tar.gz}"
			archive_version="${archive_version%_*}"
			archive_version="${archive_version%_*}"
			TARGET_VERSION="v$archive_version"
			;;
	esac
elif ((SOURCE_MODE)) || (command -v "$GO_BIN" >/dev/null 2>&1 && [ -f "$ROOT_DIR/go.mod" ]); then
	TARGET_VERSION="$(checkout_version)"
else
	TARGET_VERSION="$(resolve_latest_version || true)"
fi

if [ -n "$TARGET_VERSION" ] && [ -n "$INSTALLED_VERSION" ]; then
	if [ "$TARGET_VERSION" = "$INSTALLED_VERSION" ]; then
		if ((FORCE)); then
			echo "opencode-gateway update: reinstalling $INSTALLED_VERSION (--force)"
		elif ((DRY_RUN)); then
			echo "opencode-gateway update: already up to date ($INSTALLED_VERSION); no changes needed"
			exit 0
		else
			verify_manifest_files "$manifest"
			echo "opencode-gateway update: already up to date ($INSTALLED_VERSION)"
			exit 0
		fi
	fi
	if version_number "$TARGET_VERSION" >/dev/null 2>&1 &&
		version_number "$INSTALLED_VERSION" >/dev/null 2>&1; then
		if version_less "$(version_number "$TARGET_VERSION")" "$(version_number "$INSTALLED_VERSION")"; then
			((FORCE)) ||
				die "refusing to downgrade from $INSTALLED_VERSION to $TARGET_VERSION; use --force to proceed"
		fi
	fi
fi

if [ -n "$INSTALLED_VERSION" ] && [ -n "$TARGET_VERSION" ]; then
	echo "opencode-gateway update: updating from $INSTALLED_VERSION to $TARGET_VERSION"
elif [ -n "$INSTALLED_VERSION" ]; then
	echo "opencode-gateway update: updating $INSTALLED_VERSION in $INSTALL_DIR_VALUE"
elif [ -n "$TARGET_VERSION" ]; then
	echo "opencode-gateway update: installing $TARGET_VERSION into $INSTALL_DIR_VALUE"
else
	echo "opencode-gateway update: updating the install in $INSTALL_DIR_VALUE"
fi

if ((FORCE)); then
	DELEGATE_ARGS+=(--force)
fi
if ((DRY_RUN)); then
	DELEGATE_ARGS+=(--dry-run)
fi
exec env PREFIX="$PREFIX_VALUE" INSTALL_DIR="$INSTALL_DIR_VALUE" \
	"$ROOT_DIR/scripts/install.sh" "${DELEGATE_ARGS[@]}"
