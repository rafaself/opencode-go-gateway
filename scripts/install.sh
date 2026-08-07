#!/usr/bin/env bash
set -euo pipefail

# Install the gateway without requiring a Go toolchain when a published
# release is available. Every downloaded archive is checked against the
# release checksum manifest before extraction or installation.
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
WORK_DIR=""
INSTALL_STAGE=""
MANIFEST_NAME=".opencode-gateway.manifest"

die() {
	echo "opencode-gateway install: $*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/install.sh [options]

Install opencode-gateway and the ocgtw command without requiring root.
By default, build from this checkout when Go is available; otherwise install
the latest verified GitHub release.

Options:
  --prefix DIR             Installation prefix (default: $HOME/.local)
  --install-dir DIR        Exact directory for both binaries
  --version VERSION        Release version, for example v0.1.0
  --repository OWNER/REPO  GitHub repository (default: rafaself/opencode-go-gateway)
  --source                 Require a local source build
  --release                Require a published GitHub release
  --archive FILE           Install a local release archive
  --checksums FILE         Checksum manifest for --archive (default: sibling SHA256SUMS)
  --force                  Replace files even when they differ from the install manifest
  --dry-run                Verify and prepare the install without writing binaries
  -h, --help               Show this help

The downloaded or local archive must have a matching SHA-256 entry in
SHA256SUMS. There is no option to bypass checksum verification. After a
successful install, a manifest listing the installed files and their SHA-256
hashes is written next to the binaries; re-installs refuse to overwrite files
that differ from the manifest unless --force is used. Use
scripts/update.sh (or make update) to update to a newer version, and
scripts/uninstall.sh (or make uninstall) to remove the install deterministically.
EOF
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

cleanup() {
	if [ -n "$INSTALL_STAGE" ] && [ -d "$INSTALL_STAGE" ]; then
		rm -rf -- "$INSTALL_STAGE"
	fi
	if [ -n "$WORK_DIR" ] && [ -d "$WORK_DIR" ]; then
		rm -rf -- "$WORK_DIR"
	fi
}
trap cleanup EXIT

absolute_path() {
	case "$1" in
		/*) printf '%s\n' "$1" ;;
		*) printf '%s/%s\n' "$PWD" "$1" ;;
	esac
}

validate_repository() {
	[[ "$REPOSITORY" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] ||
		die "repository must be OWNER/REPO"
}

validate_version() {
	[[ "$VERSION_VALUE" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] ||
		die "version contains unsafe archive characters"
}

detect_target() {
	local system architecture
	system="$(uname -s)"
	architecture="$(uname -m)"
	case "$system" in
		Linux) GOOS_VALUE="linux" ;;
		Darwin) GOOS_VALUE="darwin" ;;
		*) die "unsupported operating system: $system (supported: Linux and macOS)" ;;
	esac
	case "$architecture" in
		x86_64|amd64) GOARCH_VALUE="amd64" ;;
		aarch64|arm64) GOARCH_VALUE="arm64" ;;
		*) die "unsupported architecture: $architecture (supported: amd64 and arm64)" ;;
	esac
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

verify_checksum() {
	local archive="$1"
	local checksum_file="$2"
	local archive_name expected actual matches match_count
	archive_name="$(basename -- "$archive")"
	[ -f "$checksum_file" ] && [ ! -L "$checksum_file" ] || die "checksum manifest is missing or is a symlink"
	matches="$(awk -v wanted="$archive_name" '$2 == wanted {print $1}' "$checksum_file")"
	match_count="$(printf '%s\n' "$matches" | sed '/^$/d' | wc -l | tr -d ' ')"
	[ "$match_count" -eq 1 ] ||
		die "checksum manifest does not contain exactly one entry for $archive_name"
	expected="$(printf '%s\n' "$matches" | sed -n '1p')"
	[[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] ||
		die "checksum entry for $archive_name is invalid"
	expected="$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')"
	actual="$(sha256_file "$archive")"
	[ "$actual" = "$expected" ] ||
		die "checksum verification failed for $archive_name"
}

manifest_path() {
	printf '%s/%s\n' "$INSTALL_DIR_VALUE" "$MANIFEST_NAME"
}

# verify_existing_install refuses to overwrite an install recorded by the
# manifest when its files no longer match the recorded hashes, unless --force
# is used. Files that were never installed are left untouched.
verify_existing_install() {
	local manifest="$1" name entry actual recorded_name recorded_hash
	[ -f "$manifest" ] && [ ! -L "$manifest" ] || return 0
	((FORCE)) && return 0
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
		name="$INSTALL_DIR_VALUE/$recorded_name"
		if [ -e "$name" ] || [ -L "$name" ]; then
			if [ ! -f "$name" ] || [ -L "$name" ]; then
				die "existing $recorded_name is not a regular file; use --force to replace it"
			fi
			actual="$(sha256_file "$name")"
			[ "$actual" = "$recorded_hash" ] ||
				die "existing $recorded_name differs from the install manifest; use --force to replace it"
		fi
	done < "$manifest"
}

# write_manifest records the installed files, their modes, SHA-256 hashes,
# version, and install directory next to the binaries. The manifest is
# replaced atomically, so an interrupted install never leaves a half-written
# record behind.
write_manifest() {
	local installed_dir="$1" version_output version_value commit_value stage name hash
	version_output="$("$installed_dir/opencode-gateway" version 2>&1)" ||
		die "could not read version from the installed binary"
	version_value="$(printf '%s\n' "$version_output" | sed -n 's/.*version=\([^ ]*\).*/\1/p')"
	commit_value="$(printf '%s\n' "$version_output" | sed -n 's/.*commit=\([^ ]*\).*/\1/p')"
	[ -n "$version_value" ] && [ -n "$commit_value" ] || die "installed binary reported an unparsable version"
	stage="$(mktemp "$installed_dir/.opencode-gateway-manifest.XXXXXX")"
	{
		echo "# opencode-gateway install manifest (managed; do not edit)"
		echo "version=$version_value"
		echo "commit=$commit_value"
		echo "install_dir=$installed_dir"
		for name in opencode-gateway ocgtw; do
			hash="$(sha256_file "$installed_dir/$name")" || { rm -f -- "$stage"; return 1; }
			echo "file 0755 $name $hash"
		done
	} > "$stage"
	mv -f "$stage" "$installed_dir/$MANIFEST_NAME"
}

validate_archive_entries() {
	local archive="$1"
	local archive_stem="$2"
	local entries expected entry entry_count=0
	entries="$(tar -tzf "$archive")" || die "could not read release archive"
	while IFS= read -r entry; do
		[ -z "$entry" ] && continue
		entry="$(printf '%s' "$entry" | sed 's#/$##')"
		entry_count=$((entry_count + 1))
		case "$entry" in
			"$archive_stem"|"$archive_stem/opencode-gateway"|"$archive_stem/ocgtw"|"$archive_stem/LICENSE"|"$archive_stem/README.txt") ;;
			*) die "release archive contains an unexpected path" ;;
		esac
	done <<< "$entries"
	[ "$entry_count" -eq 5 ] || die "release archive does not have the expected contents"
	for expected in \
		"$archive_stem/opencode-gateway" \
		"$archive_stem/ocgtw" \
		"$archive_stem/LICENSE" \
		"$archive_stem/README.txt"; do
		printf '%s\n' "$entries" | sed 's#/$##' | grep -F -x -- "$expected" >/dev/null ||
			die "release archive is missing an expected file"
	done
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

install_binaries() {
	local extracted_dir="$1"
	local source_binary="$extracted_dir/opencode-gateway"
	local source_alias="$extracted_dir/ocgtw"
	[ -f "$source_binary" ] && [ ! -L "$source_binary" ] ||
		die "release binary is missing or is a symlink"
	[ -f "$source_alias" ] && [ ! -L "$source_alias" ] ||
		die "ocgtw binary is missing or is a symlink"

	verify_existing_install "$(manifest_path)"

	if ((DRY_RUN)); then
		echo "Would install opencode-gateway and ocgtw into $INSTALL_DIR_VALUE"
		return
	fi
	require_command install
	install -d "$INSTALL_DIR_VALUE"
	[ ! -d "$INSTALL_DIR_VALUE/opencode-gateway" ] ||
		die "installation destination is a directory: $INSTALL_DIR_VALUE/opencode-gateway"
	[ ! -d "$INSTALL_DIR_VALUE/ocgtw" ] ||
		die "installation destination is a directory: $INSTALL_DIR_VALUE/ocgtw"
	INSTALL_STAGE="$(mktemp -d "$INSTALL_DIR_VALUE/.opencode-gateway-install.XXXXXX")"
	install -m 0755 "$source_binary" "$INSTALL_STAGE/opencode-gateway"
	install -m 0755 "$source_alias" "$INSTALL_STAGE/ocgtw"
	mv -f "$INSTALL_STAGE/opencode-gateway" "$INSTALL_DIR_VALUE/opencode-gateway"
	mv -f "$INSTALL_STAGE/ocgtw" "$INSTALL_DIR_VALUE/ocgtw"
	rmdir "$INSTALL_STAGE"
	INSTALL_STAGE=""
	write_manifest "$INSTALL_DIR_VALUE"
	echo "Installed opencode-gateway and ocgtw into $INSTALL_DIR_VALUE"
	if ! version_output="$("$INSTALL_DIR_VALUE/opencode-gateway" version 2>&1)"; then
		echo "opencode-gateway install: warning: the installed binary could not report its version: $version_output" >&2
	else
		echo "$version_output"
	fi
}

build_from_source() {
	command -v "$GO_BIN" >/dev/null 2>&1 ||
		die "Go is not on PATH; use release mode after a release is published or install Go"
	[ -f "$ROOT_DIR/go.mod" ] && [ -d "$ROOT_DIR/cmd/opencode-gateway" ] ||
		die "source mode must run from the project checkout"
	local version commit build_date ldflags source_binary
	version="$VERSION_VALUE"
	if [ -z "$version" ]; then
		version="$(git -C "$ROOT_DIR" describe --tags --always --dirty 2>/dev/null || printf 'dev')"
	fi
	commit="$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || printf 'unknown')"
	build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	[[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] ||
		die "source version contains unsafe metadata characters"
	[[ "$commit" =~ ^[A-Za-z0-9._-]+$ ]] ||
		die "source commit contains unsafe metadata characters"
	source_binary="$WORK_DIR/opencode-gateway"
	ldflags="-X main.version=$version -X main.commit=$commit -X main.buildDate=$build_date"
	"$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$source_binary" "$ROOT_DIR/cmd/opencode-gateway"
	cp "$source_binary" "$WORK_DIR/ocgtw"
	install_binaries "$WORK_DIR"
}

resolve_latest_version() {
	local latest_url
	require_command curl
	latest_url="$(curl --fail --silent --show-error --location \
		--proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 120 \
		--output /dev/null --write-out '%{url_effective}' \
		"https://github.com/$REPOSITORY/releases/latest")" ||
		die "could not resolve the latest release"
	case "$latest_url" in
		"https://github.com/$REPOSITORY/releases/tag/"*) VERSION_VALUE="${latest_url##*/}" ;;
		"https://github.com/$REPOSITORY/releases") die "no published release was found for this repository" ;;
		*) die "GitHub did not return a release URL for this repository" ;;
	esac
	validate_version
}

install_release_archive() {
	local archive checksum_file archive_name archive_stem archive_path checksum_path extract_dir version_suffix expected_archive_name
	detect_target
	if [ -n "$ARCHIVE_VALUE" ]; then
		archive_path="$(absolute_path "$ARCHIVE_VALUE")"
		[ -f "$archive_path" ] && [ ! -L "$archive_path" ] ||
			die "local release archive does not exist or is a symlink"
		archive_name="$(basename -- "$archive_path")"
		if [ -z "$VERSION_VALUE" ]; then
			printf -v version_suffix '_%s_%s.tar.gz' "$GOOS_VALUE" "$GOARCH_VALUE"
			case "$archive_name" in
				opencode-gateway_*"$version_suffix") ;;
				*) die "local archive name does not match the host target" ;;
			esac
			VERSION_VALUE="${archive_name#opencode-gateway_}"
			VERSION_VALUE="${VERSION_VALUE%"$version_suffix"}"
		fi
		validate_version
		printf -v expected_archive_name 'opencode-gateway_%s_%s_%s.tar.gz' "$VERSION_VALUE" "$GOOS_VALUE" "$GOARCH_VALUE"
		[ "$archive_name" = "$expected_archive_name" ] ||
			die "local archive name does not match the selected version and host target"
		if [ -n "$CHECKSUMS_VALUE" ]; then
			checksum_path="$(absolute_path "$CHECKSUMS_VALUE")"
		else
			checksum_path="$(dirname -- "$archive_path")/SHA256SUMS"
		fi
	else
		if [ -z "$VERSION_VALUE" ]; then
			resolve_latest_version
		else
			validate_version
		fi
		printf -v archive_name 'opencode-gateway_%s_%s_%s.tar.gz' "$VERSION_VALUE" "$GOOS_VALUE" "$GOARCH_VALUE"
		archive_path="$WORK_DIR/$archive_name"
		checksum_path="$WORK_DIR/SHA256SUMS"
		require_command curl
		curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
			--connect-timeout 10 --max-time 120 \
			--output "$archive_path" \
			"https://github.com/$REPOSITORY/releases/download/$VERSION_VALUE/$archive_name" ||
			die "could not download the release archive"
		curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
			--connect-timeout 10 --max-time 120 \
			--output "$checksum_path" \
			"https://github.com/$REPOSITORY/releases/download/$VERSION_VALUE/SHA256SUMS" ||
			die "could not download the release checksum manifest"
	fi
	archive_stem="${archive_name%.tar.gz}"
	verify_checksum "$archive_path" "$checksum_path"
	validate_archive_entries "$archive_path" "$archive_stem"
	extract_dir="$WORK_DIR/extracted"
	mkdir -p "$extract_dir"
	tar -xzf "$archive_path" -C "$extract_dir"
	[ -d "$extract_dir/$archive_stem" ] && [ ! -L "$extract_dir/$archive_stem" ] ||
		die "release archive extracted an unsafe directory"
	install_binaries "$extract_dir/$archive_stem"
}

while (($# > 0)); do
	case "$1" in
		--prefix)
			(($# >= 2)) || die "--prefix requires a value"
			PREFIX_VALUE="$2"
			shift 2
			;;
		--install-dir)
			(($# >= 2)) || die "--install-dir requires a value"
			INSTALL_DIR_VALUE="$2"
			shift 2
			;;
		--version)
			(($# >= 2)) || die "--version requires a value"
			VERSION_VALUE="$2"
			shift 2
			;;
		--repository)
			(($# >= 2)) || die "--repository requires a value"
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
			(($# >= 2)) || die "--archive requires a value"
			ARCHIVE_VALUE="$2"
			shift 2
			;;
		--checksums)
			(($# >= 2)) || die "--checksums requires a value"
			CHECKSUMS_VALUE="$2"
			shift 2
			;;
		--dry-run)
			DRY_RUN=1
			shift
			;;
		--force)
			FORCE=1
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

((SOURCE_MODE + RELEASE_MODE <= 1)) || die "use only one of --source or --release"
[ -z "$ARCHIVE_VALUE" ] || [ "$RELEASE_MODE" -eq 0 ] ||
	die "--archive cannot be combined with --release"
[ -z "$ARCHIVE_VALUE" ] || [ "$SOURCE_MODE" -eq 0 ] ||
	die "--archive cannot be combined with --source"
[ -z "$CHECKSUMS_VALUE" ] || [ -n "$ARCHIVE_VALUE" ] ||
	die "--checksums requires --archive"
validate_repository
prepare_destination
TMP_ROOT=/tmp
TMP_ENV="$(printenv TMPDIR 2>/dev/null || true)"
[ -n "$TMP_ENV" ] && [[ "$TMP_ENV" == /* ]] && TMP_ROOT="$TMP_ENV"
WORK_DIR="$(mktemp -d "$TMP_ROOT/opencode-gateway-install.XXXXXX")"

if ((SOURCE_MODE)); then
	build_from_source
elif [ -n "$ARCHIVE_VALUE" ]; then
	install_release_archive
elif ((RELEASE_MODE)); then
	install_release_archive
elif command -v "$GO_BIN" >/dev/null 2>&1 && [ -f "$ROOT_DIR/go.mod" ]; then
	build_from_source
else
	if [ -f "$ROOT_DIR/go.mod" ]; then
		echo "opencode-gateway install: Go is not on PATH; installing the latest published release instead of this checkout." >&2
		echo "opencode-gateway install: install Go and re-run, or use --source, to build the current working tree." >&2
	fi
	install_release_archive
fi
