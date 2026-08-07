#!/usr/bin/env bash

set -euo pipefail
umask 077

if [[ ${RUN_LIVE_SMOKE:-} != "1" ]]; then
	echo "live smoke is opt-in; rerun with RUN_LIVE_SMOKE=1" >&2
	exit 2
fi
if [[ -z ${OPENCODE_GO_API_KEY:-} ]]; then
	echo "OPENCODE_GO_API_KEY must be set in the gateway environment" >&2
	exit 2
fi

for command_name in codex curl git jq rg; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		echo "required command not found: $command_name" >&2
		exit 2
	fi
done

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
assistant_output_filter="$script_dir/live-smoke-assistant-output.jq"
gateway_binary="$repo_root/bin/opencode-gateway"
if [[ ! -x "$gateway_binary" ]]; then
	echo "missing executable $gateway_binary; run make build first" >&2
	exit 2
fi
if [[ ! -r "$assistant_output_filter" ]]; then
	echo "missing assistant-output filter $assistant_output_filter" >&2
	exit 2
fi

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/opencode-gateway-smoke.XXXXXX")
gateway_pid=""
codex_pid=""
reader_pid=""
smoke_succeeded=0

cleanup() {
	local exit_status=$?

	if [[ -n "$codex_pid" ]]; then
		kill "$codex_pid" 2>/dev/null || true
		wait "$codex_pid" 2>/dev/null || true
	fi
	if [[ -n "$reader_pid" ]]; then
		wait "$reader_pid" 2>/dev/null || true
	fi
	if [[ -n "$gateway_pid" ]]; then
		kill "$gateway_pid" 2>/dev/null || true
		wait "$gateway_pid" 2>/dev/null || true
	fi
	if (( smoke_succeeded == 1 && exit_status == 0 )); then
		if ! rm -rf -- "$temporary_dir"; then
			echo "live smoke passed, but failed to remove temporary run directory $temporary_dir" >&2
			return 1
		fi
	else
		echo "live smoke failed; diagnostics retained in $temporary_dir" >&2
	fi
	return "$exit_status"
}
trap cleanup EXIT INT TERM

gateway_stdout="$temporary_dir/gateway.stdout"
gateway_log="$temporary_dir/gateway.log"
codex_home="$temporary_dir/codex-home"
codex_repo="$temporary_dir/repo"
codex_output="$temporary_dir/codex.jsonl"
codex_error="$temporary_dir/codex.stderr"
codex_fifo="$temporary_dir/codex.fifo"
codex_status_file="$temporary_dir/codex.status"
first_output_marker="$temporary_dir/first-output"
prompt_marker="opencode-gateway-live-smoke-marker"

mkdir -p "$codex_home" "$codex_repo"
git -C "$codex_repo" init --quiet
git -C "$codex_repo" config user.name "Gateway Smoke Test"
git -C "$codex_repo" config user.email "gateway-smoke@example.invalid"

(
	cd "$repo_root"
	OPENCODE_GO_API_KEY="$OPENCODE_GO_API_KEY" \
	OPENCODE_GATEWAY_HOST=127.0.0.1 \
	OPENCODE_GATEWAY_PORT=0 \
	OPENCODE_GATEWAY_LOG_LEVEL=info \
	"$gateway_binary" run
) >"$gateway_stdout" 2>"$gateway_log" &
gateway_pid=$!

gateway_url=""
for _ in {1..100}; do
	gateway_url=$(sed -n 's/^opencode-gateway listening on \(http:\/\/[^ ]*\)$/\1/p' "$gateway_stdout" | tail -n 1 || true)
	if [[ -n "$gateway_url" ]] && curl --fail --silent "$gateway_url/health/ready" >/dev/null; then
		break
	fi
	sleep 0.1
done
if [[ -z "$gateway_url" ]] || ! curl --fail --silent "$gateway_url/health/ready" >/dev/null; then
	echo "gateway did not become ready; inspect the temporary smoke logs while debugging" >&2
	exit 1
fi

"$gateway_binary" version
codex --version

mkfifo "$codex_fifo"
: >"$codex_output"
(
	while IFS= read -r line; do
		printf '%s\n' "$line" >>"$codex_output"
		if [[ ! -e "$first_output_marker" ]] && jq -e -f "$assistant_output_filter" <<<"$line" >/dev/null 2>&1; then
			: >"$first_output_marker"
		fi
	done <"$codex_fifo"
) &
reader_pid=$!

(
	cd "$codex_repo"
	exec env -u OPENCODE_GO_API_KEY CODEX_HOME="$codex_home" codex exec \
		--cd "$codex_repo" \
		--ignore-user-config \
		--ephemeral \
		--skip-git-repo-check \
		--model 'deepseek-v4-flash (go)' \
		--sandbox read-only \
		--color never \
		--json \
		-c 'model_provider="gateway"' \
		-c "model_providers.gateway={name=\"Local Gateway\", base_url=\"$gateway_url/v1\", wire_api=\"responses\", request_max_retries=0, stream_max_retries=0}" \
		"Reply with exactly one short sentence. Smoke marker: $prompt_marker" \
		>"$codex_fifo" 2>"$codex_error"
) &
codex_pid=$!

incremental_seen=0
for _ in {1..1200}; do
	if [[ -e "$first_output_marker" ]] && kill -0 "$codex_pid" 2>/dev/null; then
		incremental_seen=1
		break
	fi
	if ! kill -0 "$codex_pid" 2>/dev/null; then
		break
	fi
	sleep 0.1
done
if (( incremental_seen == 0 )); then
	kill "$codex_pid" 2>/dev/null || true
	wait "$codex_pid" 2>/dev/null || true
	codex_pid=""
	echo "Codex produced no output while the process was still running" >&2
	exit 1
fi

set +e
wait "$codex_pid"
codex_status=$?
set -e
codex_pid=""
printf '%s\n' "$codex_status" >"$codex_status_file"
wait "$reader_pid"
reader_pid=""

codex_status=$(<"$codex_status_file")
if [[ "$codex_status" != "0" ]]; then
	echo "Codex smoke request failed; stderr was retained only in the temporary run directory" >&2
	exit 1
fi
event_count=$(jq -s 'length' "$codex_output")
if (( event_count < 3 )); then
	echo "Codex JSONL contained too few events to prove incremental output" >&2
	exit 1
fi
if ! jq -s -e '.[-1].type == "turn.completed"' "$codex_output" >/dev/null; then
	echo "Codex JSONL did not end with turn.completed" >&2
	exit 1
fi
if ! jq -s -e -f "$assistant_output_filter" "$codex_output" >/dev/null; then
	echo "Codex JSONL did not contain an assistant output event" >&2
	exit 1
fi

if ! grep -Fq 'response_model=deepseek-v4-flash' "$gateway_log"; then
	echo "gateway logs did not confirm model deepseek-v4-flash" >&2
	exit 1
fi
if ! grep -Fq 'response_terminal=response.completed' "$gateway_log"; then
	echo "gateway logs did not confirm response.completed" >&2
	exit 1
fi
for forbidden in "$OPENCODE_GO_API_KEY" "$prompt_marker" "Authorization" "Bearer "; do
	if [[ -n "$forbidden" ]] && grep -Fq -- "$forbidden" "$gateway_log"; then
		echo "gateway logs contained a forbidden secret or request value" >&2
		exit 1
	fi
done
if rg -F --hidden --glob '!*.lock' -- "$OPENCODE_GO_API_KEY" "$codex_home" "$codex_repo" >/dev/null 2>&1; then
	echo "the OpenCode Go key was written into the temporary Codex home or repository" >&2
	exit 1
fi

smoke_succeeded=1
echo "live smoke passed: incremental Codex JSONL, turn.completed, deepseek-v4-flash, and secret-free gateway logs verified"
