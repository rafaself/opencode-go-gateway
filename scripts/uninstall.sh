#!/usr/bin/env bash
set -euo pipefail

# Deterministically remove an opencode-gateway install. Only files recorded in
# the install manifest are touched, and only when their SHA-256 still matches
# the recorded value; a modified or replaced file is never deleted without
# --force. Codex configuration is never modified unless --codex-home is given,
# in which case the most recent setup backup is restored first.
umask 077

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
PREFIX_VALUE="${PREFIX:-}"
INSTALL_DIR_VALUE="${INSTALL_DIR:-}"
CODEX_HOME_VALUE=""
DRY_RUN=0
FORCE=0
MANIFEST_NAME=".opencode-gateway.manifest"

die() {
	echo "opencode-gateway uninstall: $*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/uninstall.sh [options]

Deterministically remove opencode-gateway and the ocgtw command. Files are
removed only when they match the hashes recorded in the install manifest
written by scripts/install.sh; anything modified since install is left in
place unless --force is used.

Options:
  --prefix DIR             Installation prefix (default: $HOME/.local)
  --install-dir DIR        Exact directory containing the binaries
  --codex-home DIR         Before uninstalling, restore the most recent
                           opencode-gateway setup backup in this Codex home
  --force                  Remove modified files and remove the canonical
                           binaries even when no manifest exists
  --dry-run                Show what would be removed without changing anything
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

# restore_latest_codex_backup invokes the still-installed gateway CLI to roll
# back the most recent setup backup in the selected Codex home. It runs before
# any binary is removed so the restore always uses the recorded install.
restore_latest_codex_backup() {
	local codex_home="$1" binary="$2" latest backup_count
	[ -d "$codex_home" ] || die "--codex-home is not a directory: $codex_home"
	[[ "$codex_home" == /* ]] || die "--codex-home must be an absolute path"
	latest="$(find "$codex_home" -maxdepth 1 -type d -name 'backup-opencode-gateway-*' -print 2>/dev/null | sort | tail -n 1)"
	[ -n "$latest" ] || {
		echo "opencode-gateway uninstall: no setup backup found under $codex_home; leaving Codex configuration untouched"
		return 0
	}
	if ((DRY_RUN)); then
		echo "Would restore Codex setup backup: $latest"
		return 0
	fi
	echo "Restoring Codex setup backup: $latest"
	"$binary" setup codex --codex-home "$codex_home" --restore "$latest"
}

# collect_removals reads the manifest and verifies every recorded file against
# its recorded hash. The result is stored in REMOVE_PATHS (one path per line)
# and REMOVE_DESCRIPTIONS (one display line per file).
collect_removals() {
	local manifest="$1" entry name hash path actual
	REMOVE_PATHS=""
	REMOVE_DESCRIPTIONS=""
	if [ ! -f "$manifest" ] || [ -L "$manifest" ]; then
		if ((FORCE)); then
			for name in opencode-gateway ocgtw; do
				path="$INSTALL_DIR_VALUE/$name"
				if [ -e "$path" ] || [ -L "$path" ]; then
					REMOVE_PATHS="$REMOVE_PATHS
$path"
					REMOVE_DESCRIPTIONS="$REMOVE_DESCRIPTIONS
remove $path"
				fi
			done
			return 0
		fi
		die "no install manifest found at $manifest; refusing to guess what to remove (use --force to remove the canonical binaries)"
	fi
	manifest_install_dir=""
	manifest_version=""
	while IFS= read -r entry; do
		[ -n "$entry" ] && [ "${entry#\#}" = "$entry" ] || continue
		case "$entry" in
			install_dir=*)
				manifest_install_dir="${entry#install_dir=}"
				;;
			version=*)
				manifest_version="${entry#version=}"
				;;
			file\ *)
				set -- $entry
				name="$3"
				hash="$4"
				[ -n "$name" ] && [ -n "$hash" ] || die "manifest contains an invalid file entry"
				path="$INSTALL_DIR_VALUE/$name"
				if [ ! -e "$path" ] && [ ! -L "$path" ]; then
					REMOVE_DESCRIPTIONS="$REMOVE_DESCRIPTIONS
missing (already removed) $path"
					continue
				fi
				[ -f "$path" ] && [ ! -L "$path" ] || die "installed $name is not a regular file; use --force to remove it"
				actual="$(sha256_file "$path")"
				if [ "$actual" != "$hash" ]; then
					((FORCE)) ||
						die "installed $name differs from the install manifest; use --force to remove it"
				fi
				REMOVE_PATHS="$REMOVE_PATHS
$path"
				REMOVE_DESCRIPTIONS="$REMOVE_DESCRIPTIONS
remove $path"
				;;
		esac
	done < "$manifest"
	[ -n "$manifest_install_dir" ] || die "manifest is missing install_dir"
	[ -n "$manifest_version" ] || die "manifest is missing version"
	if [ "$manifest_install_dir" != "$INSTALL_DIR_VALUE" ]; then
		die "manifest records install dir $manifest_install_dir, not $INSTALL_DIR_VALUE; pass the matching --install-dir"
	fi
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
		--codex-home)
			[ "$#" -ge 2 ] || die "--codex-home requires a value"
			CODEX_HOME_VALUE="$2"
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
binary_path="$INSTALL_DIR_VALUE/opencode-gateway"
CODEX_RESTORED=0
manifest_version=""
manifest="$(printf '%s/%s\n' "$INSTALL_DIR_VALUE" "$MANIFEST_NAME")"
collect_removals "$manifest"

if [ -n "$CODEX_HOME_VALUE" ]; then
	if [ -f "$binary_path" ] && [ ! -L "$binary_path" ]; then
		restore_latest_codex_backup "$CODEX_HOME_VALUE" "$binary_path"
		CODEX_RESTORED=1
	else
		die "cannot restore Codex setup: the installed gateway binary is missing at $binary_path"
	fi
fi

if ((DRY_RUN)); then
	printf '%s\n' "$REMOVE_DESCRIPTIONS" | sed '/^$/d'
	echo "Would remove the install manifest: $manifest"
	exit 0
fi

if [ -n "$REMOVE_PATHS" ]; then
	while IFS= read -r path; do
		rm -f -- "$path"
	done <<< "$REMOVE_PATHS"
fi
[ ! -e "$manifest" ] || rm -f -- "$manifest"
printf '%s\n' "$REMOVE_DESCRIPTIONS" | sed '/^$/d'
if [ -n "$manifest_version" ]; then
	echo "Uninstalled opencode-gateway and ocgtw (version $manifest_version) from $INSTALL_DIR_VALUE"
else
	echo "Uninstalled opencode-gateway and ocgtw from $INSTALL_DIR_VALUE"
fi
if ((CODEX_RESTORED)); then
	echo "The most recent Codex setup backup was restored."
else
	echo "Codex configuration is left untouched; roll back a Codex setup with: opencode-gateway setup codex --restore /path/to/backup"
fi
