#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN="${GO:-go}"
OUTPUT_DIR="${OUTPUT_DIR:-dist}"
VERSION_VALUE="${VERSION:-}"
COMMIT_VALUE="${COMMIT:-}"
BUILD_DATE_VALUE="${BUILD_DATE:-}"
SOURCE_DATE_EPOCH_VALUE="${SOURCE_DATE_EPOCH:-}"
SELF_TEST=0

TARGETS=(
	linux/amd64
	linux/arm64
	darwin/amd64
	darwin/arm64
	windows/amd64
)

usage() {
	cat <<'EOF'
Usage: scripts/package-release.sh [options]

Build and package the supported OpenCode Gateway release targets.

Options:
  --output-dir DIR          Output directory (default: dist)
  --version VERSION         Version embedded in binaries and archive names
  --commit COMMIT           Commit metadata embedded in binaries
  --build-date RFC3339      UTC build date embedded in binaries
  --source-date-epoch SEC   Reproducible archive timestamp (default: HEAD time)
  --self-test               Verify checksums and archive contents after packaging
  -h, --help                Show this help
EOF
}

die() {
	echo "package-release: $*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

utc_date_from_epoch() {
	local epoch="$1"
	date -u -d "@${epoch}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || \
		date -u -r "${epoch}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || \
		die "cannot format SOURCE_DATE_EPOCH with this date implementation"
}

set_file_mtime() {
	local path="$1"
	if touch -d "@${SOURCE_DATE_EPOCH_VALUE}" "$path" 2>/dev/null; then
		return
	fi
	local timestamp
	timestamp="$(date -u -d "@${SOURCE_DATE_EPOCH_VALUE}" +%Y%m%d%H%M.%S 2>/dev/null || date -u -r "${SOURCE_DATE_EPOCH_VALUE}" +%Y%m%d%H%M.%S 2>/dev/null)" || \
		die "cannot set reproducible file timestamps"
	touch -t "$timestamp" "$path"
}

sha256_file() {
	local path="$1"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$path" | cut -d ' ' -f 1
		return
	fi
	shasum -a 256 "$path" | cut -d ' ' -f 1
}

archive_entries() {
	local archive="$1"
	case "$archive" in
		*.zip)
			unzip -Z1 "$archive"
			;;
		*.tar.gz)
			tar -tzf "$archive"
			;;
		*)
			die "unknown archive format: $archive"
			;;
	esac
}

verify_checksum_file() {
	local checksum_file="$1"
	local expected name actual
	while read -r expected name; do
		[[ -z "${expected:-}" ]] && continue
		[[ -n "${name:-}" ]] || die "invalid checksum line"
		[[ "$name" != */* ]] || die "checksum contains an unsafe path: $name"
		actual="$(sha256_file "$(dirname -- "$checksum_file")/$name")"
		[[ "$actual" == "$expected" ]] || die "checksum mismatch: $name"
	done < "$checksum_file"
}

verify_archive_contents() {
	local archive="$1"
	local stem="$2"
	local binary_name="$3"
	local entries
	entries="$(archive_entries "$archive")"
	for expected in "$stem/$binary_name" "$stem/LICENSE" "$stem/README.txt"; do
		printf '%s\n' "$entries" | grep -F -x -- "$expected" >/dev/null || \
			die "archive $archive is missing $expected"
	done
}

while (($# > 0)); do
	case "$1" in
		--output-dir)
			(($# >= 2)) || die "--output-dir requires a value"
			OUTPUT_DIR="$2"
			shift 2
			;;
		--version)
			(($# >= 2)) || die "--version requires a value"
			VERSION_VALUE="$2"
			shift 2
			;;
		--commit)
			(($# >= 2)) || die "--commit requires a value"
			COMMIT_VALUE="$2"
			shift 2
			;;
		--build-date)
			(($# >= 2)) || die "--build-date requires a value"
			BUILD_DATE_VALUE="$2"
			shift 2
			;;
		--source-date-epoch)
			(($# >= 2)) || die "--source-date-epoch requires a value"
			SOURCE_DATE_EPOCH_VALUE="$2"
			shift 2
			;;
		--self-test)
			SELF_TEST=1
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

cd "$ROOT_DIR"
require_command "$GO_BIN"
require_command tar
require_command unzip
require_command zip
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	die "required command not found: sha256sum or shasum"
fi

if [[ -z "$VERSION_VALUE" ]]; then
	VERSION_VALUE="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi
if [[ -z "$COMMIT_VALUE" ]]; then
	COMMIT_VALUE="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
fi
if [[ -z "$SOURCE_DATE_EPOCH_VALUE" ]]; then
	SOURCE_DATE_EPOCH_VALUE="$(git show -s --format=%ct HEAD 2>/dev/null || date +%s)"
fi
if [[ -z "$BUILD_DATE_VALUE" ]]; then
	BUILD_DATE_VALUE="$(utc_date_from_epoch "$SOURCE_DATE_EPOCH_VALUE")"
fi

[[ "$VERSION_VALUE" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "version contains unsafe archive characters"
[[ "$COMMIT_VALUE" =~ ^[A-Za-z0-9._-]+$ ]] || die "commit contains unsafe metadata characters"
[[ "$SOURCE_DATE_EPOCH_VALUE" =~ ^[0-9]+$ ]] || die "source date epoch must be an integer"

if [[ "$OUTPUT_DIR" != /* ]]; then
	OUTPUT_DIR="$ROOT_DIR/$OUTPUT_DIR"
fi
mkdir -p "$OUTPUT_DIR"

temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/opencode-gateway-package.XXXXXX")"
trap 'rm -rf "$temporary_dir"' EXIT
ldflags="-s -w -X main.version=${VERSION_VALUE} -X main.commit=${COMMIT_VALUE} -X main.buildDate=${BUILD_DATE_VALUE}"
archive_paths=()

for target in "${TARGETS[@]}"; do
	goos="${target%/*}"
	goarch="${target#*/}"
	archive_stem="opencode-gateway_${VERSION_VALUE}_${goos}_${goarch}"
	stage_dir="$temporary_dir/$archive_stem"
	mkdir -p "$stage_dir"
	binary_name="opencode-gateway"
	archive_suffix=".tar.gz"
	if [[ "$goos" == "windows" ]]; then
		binary_name="opencode-gateway.exe"
		archive_suffix=".zip"
	fi
	binary_path="$stage_dir/$binary_name"

	echo "building $goos/$goarch"
	GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 "$GO_BIN" build \
		-trimpath -buildvcs=false -ldflags "$ldflags" -o "$binary_path" ./cmd/opencode-gateway
	cp "$ROOT_DIR/LICENSE" "$stage_dir/LICENSE"
	cp "$ROOT_DIR/packaging/README.txt" "$stage_dir/README.txt"
	chmod 0755 "$binary_path"
	chmod 0644 "$stage_dir/LICENSE" "$stage_dir/README.txt"
	set_file_mtime "$binary_path"
	set_file_mtime "$stage_dir/LICENSE"
	set_file_mtime "$stage_dir/README.txt"

	archive_name="${archive_stem}${archive_suffix}"
	temporary_archive="$temporary_dir/$archive_name"
	if [[ "$archive_suffix" == ".zip" ]]; then
		(
			cd "$temporary_dir"
			find "$archive_stem" -type f -print | LC_ALL=C sort | zip -X -q "$temporary_archive" -@
		)
	else
		(
			cd "$temporary_dir"
			tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@${SOURCE_DATE_EPOCH_VALUE}" -czf "$temporary_archive" "$archive_stem"
		)
	fi
	mv -f "$temporary_archive" "$OUTPUT_DIR/$archive_name"
	archive_paths+=("$OUTPUT_DIR/$archive_name")
done

checksum_file="$OUTPUT_DIR/SHA256SUMS"
temporary_checksums="$temporary_dir/SHA256SUMS"
(
	cd "$OUTPUT_DIR"
	for archive in "${archive_paths[@]}"; do
		name="$(basename -- "$archive")"
		printf '%s  %s\n' "$(sha256_file "$archive")" "$name"
	done
) | LC_ALL=C sort -k2,2 > "$temporary_checksums"
mv -f "$temporary_checksums" "$checksum_file"
set_file_mtime "$checksum_file"

if ((SELF_TEST)); then
	verify_checksum_file "$checksum_file"
	for target_index in "${!TARGETS[@]}"; do
		target="${TARGETS[$target_index]}"
		goos="${target%/*}"
		goarch="${target#*/}"
		stem="opencode-gateway_${VERSION_VALUE}_${goos}_${goarch}"
		binary_name="opencode-gateway"
		archive_suffix=".tar.gz"
		if [[ "$goos" == "windows" ]]; then
			binary_name="opencode-gateway.exe"
			archive_suffix=".zip"
		fi
		verify_archive_contents "$OUTPUT_DIR/${stem}${archive_suffix}" "$stem" "$binary_name"
	done
	echo "package self-test passed"
fi

echo "created release artifacts in $OUTPUT_DIR"
