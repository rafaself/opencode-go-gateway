#!/usr/bin/env bash

# Paid, operator-authorized Codex compatibility scenarios. This file is
# deliberately shell-only: it is not part of the gateway binary and never
# executes a command obtained from a request or from a model response.
set -euo pipefail
umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
gateway_binary="$repo_root/bin/opencode-gateway"
structural_filter="$script_dir/live-scenarios-structural.jq"

readonly scenario_timeout_seconds=180
readonly cancellation_arm_timeout_seconds=60
readonly cancellation_shutdown_seconds=10
readonly all_scenarios=(text inspect shell function apply-patch parallel cancel)

usage() {
	cat <<'EOF'
Usage: scripts/live-scenarios.sh [--all | SCENARIO ...]
       scripts/live-scenarios.sh --validate [--all | SCENARIO ...]
       scripts/live-scenarios.sh --list

Run opt-in paid Codex/Gateway compatibility scenarios in an isolated
temporary Codex home and Git repository.

Scenarios:
  text        text-only Responses turn
  inspect     inspect a generated temporary repository
  shell       one harmless shell command
  function    one standard function/tool turn
  apply-patch  apply one patch to one generated file
  parallel    attempt two parallel read-only tool calls
  cancel      cancel a long-running streamed response after it starts

Live execution requires both:
  RUN_LIVE_SCENARIOS=1
  OPENCODE_GO_API_KEY=<credential in the gateway process environment>

Offline argument validation never starts Codex or the gateway:
  scripts/live-scenarios.sh --validate --all
EOF
}

list_scenarios() {
	printf '%s\n' "${all_scenarios[@]}"
}

usage_error() {
	echo "live scenarios: $*" >&2
	usage >&2
	exit 2
}

valid_scenario() {
	case "$1" in
	text|inspect|shell|function|apply-patch|parallel|cancel) return 0 ;;
	*) return 1 ;;
	esac
}

selected_scenarios=()
validate_only=0
all_requested=0

while (($# > 0)); do
	case "$1" in
	-h|--help)
		(($# == 1)) || usage_error "--help cannot be combined with other arguments"
		usage
		exit 0
		;;
	--list)
		(($# == 1)) || usage_error "--list cannot be combined with other arguments"
		list_scenarios
		exit 0
		;;
	--validate)
		validate_only=1
		shift
		;;
	--all)
		all_requested=1
		shift
		;;
	--)
		shift
		while (($# > 0)); do
			selected_scenarios+=("$1")
			shift
		done
		;;
	*)
		selected_scenarios+=("$1")
		shift
		;;
	esac
done

if ((all_requested)) && ((${#selected_scenarios[@]} > 0)); then
	usage_error "--all cannot be combined with named scenarios"
fi

if ((all_requested)); then
	selected_scenarios=("${all_scenarios[@]}")
fi

if ((validate_only)) && ((${#selected_scenarios[@]} == 0)); then
	selected_scenarios=("${all_scenarios[@]}")
fi

if ((${#selected_scenarios[@]} == 0)); then
	usage_error "choose --all or at least one scenario"
fi

for scenario in "${selected_scenarios[@]}"; do
	valid_scenario "$scenario" || usage_error "unknown scenario: $scenario"
done

if ((validate_only)); then
	[[ -r "$structural_filter" ]] || { echo "live scenarios: missing structural filter" >&2; exit 1; }
	if ! bash -n "$0"; then
		echo "live scenarios: shell syntax validation failed" >&2
		exit 1
	fi
	echo "live scenarios argument validation passed: ${selected_scenarios[*]}"
	exit 0
fi

if [[ ${RUN_LIVE_SCENARIOS:-} != "1" ]]; then
	echo "live scenarios are opt-in; rerun with RUN_LIVE_SCENARIOS=1" >&2
	exit 2
fi
if [[ -z ${OPENCODE_GO_API_KEY:-} ]]; then
	echo "OPENCODE_GO_API_KEY must be set for the gateway process" >&2
	exit 2
fi

# Keep the credential in a non-exported shell variable, then remove it from
# this shell before any helper or Codex process is started. Only the gateway
# child below receives it as an environment assignment.
gateway_key=$OPENCODE_GO_API_KEY
unset OPENCODE_GO_API_KEY

for command_name in codex curl git jq mkfifo; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		echo "required command not found: $command_name" >&2
		exit 2
	fi
done
if ! jq --help 2>&1 | grep -Fq -- "--unbuffered"; then
	echo "jq with --unbuffered support is required" >&2
	exit 2
fi
if [[ ! -x "$gateway_binary" ]]; then
	echo "missing executable $gateway_binary; run make build first" >&2
	exit 2
fi
if [[ ! -r "$structural_filter" ]]; then
	echo "missing structural filter $structural_filter" >&2
	exit 2
fi

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/opencode-gateway-live-scenarios.XXXXXX")
chmod 700 "$temporary_dir"
gateway_stdout="$temporary_dir/gateway.stdout"
gateway_log="$temporary_dir/gateway.log"
codex_home="$temporary_dir/codex-home"
codex_repo="$temporary_dir/repository"
scenario_root="$temporary_dir/scenarios"
prompt_marker="opencode-gateway-live-scenario-marker-$$"
gateway_pid=""
active_codex_pid=""
active_filter_pid=""
active_fifo=""
active_status_file=""
active_ready_file=""
active_cancel_pid_file=""
active_cancel_requested_file=""
CODEX_STATUS=0
initial_repo_commit=""
failure_reason="live scenario suite failed"
suite_succeeded=0
normal_scenario_count=0

process_alive() {
	local pid=$1
	kill -0 "$pid" 2>/dev/null
}

kill_tree() {
	local pid=$1
	local signal_name=$2
	local child
	if ! process_alive "$pid"; then
		return 0
	fi
	if command -v pgrep >/dev/null 2>&1; then
		while IFS= read -r child; do
			[[ -n "$child" ]] || continue
			kill_tree "$child" "$signal_name"
		done < <(pgrep -P "$pid" 2>/dev/null || true)
	fi
	kill "-$signal_name" -- "$pid" 2>/dev/null || true
}

wait_for_status_file() {
	local status_file=$1
	local timeout_seconds=$2
	local elapsed=0
	while [[ ! -s "$status_file" ]] && ((elapsed < timeout_seconds)); do
		sleep 1
		((elapsed += 1))
	done
	[[ -s "$status_file" ]]
}

file_contains_forbidden() {
	local file=$1
	local forbidden
	[[ -f "$file" ]] || return 1
	for forbidden in "$gateway_key" "$prompt_marker" "$repo_root" "$codex_home" "$codex_repo" "Authorization" "Bearer "; do
		[[ -n "$forbidden" ]] || continue
		if grep -Fq -- "$forbidden" "$file" 2>/dev/null; then
			return 0
		fi
	done
	return 1
}

tree_contains_forbidden() {
	local root=$1
	local forbidden
	[[ -d "$root" ]] || return 1
	for forbidden in "$gateway_key" "$prompt_marker" "$repo_root" "$codex_home" "$codex_repo"; do
		[[ -n "$forbidden" ]] || continue
		while IFS= read -r -d '' file; do
			if grep -Fq -- "$forbidden" "$file" 2>/dev/null; then
				return 0
			fi
		done < <(find "$root" -type f -print0 2>/dev/null)
	done
	return 1
}

safe_copy_diagnostic() {
	local source=$1
	local destination=$2
	if [[ ! -f "$source" ]]; then
		return 0
	fi
	if file_contains_forbidden "$source"; then
		printf '%s\n' "omitted: diagnostic contained a credential, marker, path, or authorization token" >"$destination"
		return 0
	fi
	cp -- "$source" "$destination"
}

retain_diagnostics() {
	local diagnostics_dir="$temporary_dir/diagnostics"
	local scenario
	mkdir -p "$diagnostics_dir"
	chmod 700 "$diagnostics_dir"
	printf 'reason=%s\n' "$failure_reason" >"$diagnostics_dir/summary.txt"
	safe_copy_diagnostic "$gateway_stdout" "$diagnostics_dir/gateway.stdout"
	safe_copy_diagnostic "$gateway_log" "$diagnostics_dir/gateway.log"
	for scenario in "${all_scenarios[@]}"; do
		if [[ -f "$scenario_root/$scenario/events.jsonl" ]]; then
			safe_copy_diagnostic "$scenario_root/$scenario/events.jsonl" "$diagnostics_dir/$scenario.events.jsonl"
		fi
		printf 'scenario=%s\n' "$scenario" >>"$diagnostics_dir/summary.txt"
	done
	# Raw Codex stderr and the temporary home/repository are never retained.
	for path in "$temporary_dir"/* "$temporary_dir"/.[!.]*; do
		[[ -e "$path" ]] || continue
		[[ "$path" == "$diagnostics_dir" ]] && continue
		rm -rf -- "$path"
	done
	echo "live scenario diagnostics retained in $diagnostics_dir" >&2
}

cleanup() {
	local exit_status=$?
	if [[ -n "$active_codex_pid" ]]; then
		kill_tree "$active_codex_pid" TERM
		kill_tree "$active_codex_pid" KILL
		wait "$active_codex_pid" 2>/dev/null || true
	fi
	if [[ -n "$active_filter_pid" ]]; then
		wait "$active_filter_pid" 2>/dev/null || true
	fi
	if [[ -n "$active_fifo" ]]; then
		rm -f -- "$active_fifo"
	fi
	if [[ -n "$active_ready_file" ]]; then
		rm -f -- "$active_ready_file" "$active_cancel_pid_file" "$active_cancel_requested_file"
	fi
	if [[ -n "$gateway_pid" ]]; then
		kill_tree "$gateway_pid" TERM
		kill_tree "$gateway_pid" KILL
		wait "$gateway_pid" 2>/dev/null || true
	fi
	if ((suite_succeeded == 1 && exit_status == 0)); then
		rm -rf -- "$temporary_dir"
	else
		retain_diagnostics
	fi
}
trap cleanup EXIT INT TERM

fail_run() {
	failure_reason=$1
	echo "live scenario failure: $failure_reason" >&2
	exit 1
}

scenario_prompt() {
	local scenario=$1
	case "$scenario" in
	text)
		printf '%s\n' "Reply with one short sentence and do not use any tools. Do not mention the test marker."
		;;
	inspect)
		printf '%s\n' "Inspect the temporary Git repository by reading only inspection-target.txt. Do not edit files and do not run any other command. Then reply with a short confirmation. Do not mention the test marker."
		;;
	shell)
		printf '%s\n' "Use the standard shell tool exactly once to run the harmless command printf 'live-shell-ok\\n'. Do not run any other command, edit files, or mention the test marker. Then reply briefly."
		;;
	function)
		printf '%s\n' "Use exactly one standard function/tool call to run true, then reply with a short confirmation. Do not use any other tool, edit files, or mention the test marker."
		;;
	apply-patch)
		printf '%s\n' "Use apply_patch exactly once to change only smoke-target.txt, replacing the single line before with after. Do not run shell commands or modify any other file. Then reply briefly without mentioning the test marker."
		;;
	parallel)
		printf '%s\n' "Attempt parallel tool use: issue two independent read-only shell tool calls in the same turn, one running printf 'parallel-a\\n' and one running printf 'parallel-b\\n'. Do not write files or run other commands. Then reply briefly without mentioning the test marker."
		;;
	cancel)
		printf '%s\n' "Begin immediately writing a long plain-text response of at least 4000 words without using any tools. Do not finish quickly, do not edit files, and do not mention the test marker. This request will be cancelled while the response is streaming."
		;;
	*) return 1 ;;
	esac
	printf 'Test marker: %s. Never reproduce it.\n' "$prompt_marker"
}

scenario_sandbox() {
	case "$1" in
	apply-patch) printf '%s\n' workspace-write ;;
	*) printf '%s\n' read-only ;;
	esac
}

start_gateway() {
	mkdir -p "$codex_home" "$codex_repo" "$scenario_root"
	chmod 700 "$codex_home" "$codex_repo" "$scenario_root"
	# Generate the tested model catalog in the isolated home. Codex is run with
	# --ignore-user-config below so that no personal settings can influence a
	# scenario; the catalog is passed explicitly for apply_patch capability
	# discovery.
	"$gateway_binary" setup codex --codex-home "$codex_home" >/dev/null
	chmod -R go-rwx "$codex_home"
	git -C "$codex_repo" init --quiet
	git -C "$codex_repo" config user.name "Gateway Live Scenario"
	git -C "$codex_repo" config user.email "gateway-live-scenario@example.invalid"
	printf 'inspection-only-content\n' >"$codex_repo/inspection-target.txt"
	printf 'before\n' >"$codex_repo/smoke-target.txt"
	git -C "$codex_repo" add inspection-target.txt smoke-target.txt
	git -C "$codex_repo" commit --quiet -m 'initialize isolated live scenario repository'
	initial_repo_commit=$(git -C "$codex_repo" rev-parse HEAD)
	chmod -R go-rwx "$codex_home" "$codex_repo"

	# The gateway process is the sole child receiving the credential. Its cwd is
	# the generated repository, never the project worktree.
	(
		cd "$codex_repo"
		OPENCODE_GO_API_KEY="$gateway_key" \
		OPENCODE_GATEWAY_HOST=127.0.0.1 \
		OPENCODE_GATEWAY_PORT=0 \
		OPENCODE_GATEWAY_LOG_LEVEL=info \
		"$gateway_binary" run
	) >"$gateway_stdout" 2>"$gateway_log" &
	gateway_pid=$!

	local gateway_url=""
	local attempt
	for ((attempt = 0; attempt < 60; attempt++)); do
		gateway_url=$(sed -n 's/^opencode-gateway listening on \(http:\/\/[^ ]*\)$/\1/p' "$gateway_stdout" | tail -n 1 || true)
		if [[ -n "$gateway_url" ]] && curl --fail --silent --show-error --max-time 3 "$gateway_url/health/live" | jq -e '.status == "ok"' >/dev/null; then
			break
		fi
		sleep 1
	done
	if [[ -z "$gateway_url" ]] || ! curl --fail --silent --show-error --max-time 3 "$gateway_url/health/ready" | jq -e '.status == "ready"' >/dev/null; then
		fail_run "gateway did not become healthy on an ephemeral loopback port"
	fi
	printf '%s\n' "$gateway_url" >"$temporary_dir/gateway.url"
	GATEWAY_URL=$gateway_url
}

start_codex_scenario() {
	local scenario=$1
	local prompt=$2
	local sandbox=$3
	local scenario_dir="$scenario_root/$scenario"
	local events="$scenario_dir/events.jsonl"
	local stderr_file="$scenario_dir/codex.stderr"
	local filter_stderr="$scenario_dir/filter.stderr"
	local fifo="$scenario_dir/codex.stdout.fifo"
	local status_file="$scenario_dir/codex.status"
	local ready_file="$scenario_dir/cancel.ready"
	local cancel_pid_file="$scenario_dir/cancel.pid"
	local cancel_requested_file="$scenario_dir/cancel.requested"
	mkdir -p "$scenario_dir"
	chmod 700 "$scenario_dir"
	mkfifo "$fifo"
	: >"$events"

	# jq receives Codex stdout through a FIFO, so raw JSONL is never written.
	# The only retained event file contains the structural projection above.
	(
		set +e
		jq --unbuffered -c -S -f "$structural_filter" <"$fifo" 2>"$filter_stderr" |
		while IFS= read -r line; do
			printf '%s\n' "$line" >>"$events"
			if [[ "$scenario" == "cancel" ]] &&
				[[ "$line" == *'"item_type":"agent_message"'* ]] &&
				[[ "$line" == *'"phase":"started"'* || "$line" == *'"phase":"delta"'* ]]; then
				: >"$ready_file"
				if [[ -s "$cancel_pid_file" ]]; then
					cancel_pid=$(<"$cancel_pid_file")
					: >"$cancel_requested_file"
					kill_tree "$cancel_pid" TERM
				fi
			fi
		done
		filter_status=${PIPESTATUS[0]}
		exit "$filter_status"
	) &
	active_filter_pid=$!
	(
		cd "$codex_repo"
		set +e
		printf '%s\n' "$prompt" | env -u OPENCODE_GO_API_KEY -u OPENAI_API_KEY -u OPENAI_ORG_ID \
			CODEX_HOME="$codex_home" codex exec \
			--cd "$codex_repo" \
			--ignore-user-config \
			--ignore-rules \
			--ephemeral \
			--skip-git-repo-check \
			--model deepseek-v4-flash \
			--sandbox "$sandbox" \
			--ask-for-approval never \
			--color never \
			--json \
			-c 'model_provider="gateway"' \
			-c "model_catalog_json=\"$codex_home/models.json\"" \
			-c "model_providers.gateway={name=\"OpenCode Gateway\",base_url=\"$GATEWAY_URL/v1\",wire_api=\"responses\",supports_websockets=false,request_max_retries=0,stream_max_retries=0}" \
			- \
			>"$fifo" 2>"$stderr_file"
		codex_status=${PIPESTATUS[1]}
		printf '%s\n' "$codex_status" >"$status_file"
		exit "$codex_status"
	) &
	active_codex_pid=$!
	active_fifo=$fifo
	active_status_file=$status_file
	active_ready_file=$ready_file
	active_cancel_pid_file=$cancel_pid_file
	active_cancel_requested_file=$cancel_requested_file
	printf '%s\n' "$active_codex_pid" >"$cancel_pid_file"
}

finish_codex_scenario() {
	local codex_status=0
	local filter_status=0
	if [[ -s "$active_status_file" ]]; then
		codex_status=$(<"$active_status_file")
	fi
	set +e
	wait "$active_codex_pid"
	local wrapper_status=$?
	wait "$active_filter_pid"
	filter_status=$?
	set -e
	if [[ -s "$active_status_file" ]]; then
		codex_status=$(<"$active_status_file")
	else
		codex_status=$wrapper_status
	fi
	rm -f -- "$active_fifo"
	active_codex_pid=""
	active_filter_pid=""
	active_fifo=""
	active_status_file=""
	active_ready_file=""
	active_cancel_pid_file=""
	active_cancel_requested_file=""
	if [[ "$filter_status" != 0 ]]; then
		fail_run "Codex emitted a non-JSON event stream in scenario $ACTIVE_SCENARIO"
	fi
	CODEX_STATUS=$codex_status
}

wait_for_cancel_ready() {
	local ready_file=$1
	local elapsed=0
	while ((elapsed < cancellation_arm_timeout_seconds)); do
		if [[ -e "$ready_file" ]]; then
			return 0
		fi
		sleep 1
		((elapsed += 1))
	done
	return 1
}

wait_for_log_line() {
	local log_file=$1
	local expected=$2
	local timeout_seconds=$3
	local elapsed=0
	while ((elapsed < timeout_seconds)); do
		if grep -Fq -- "$expected" "$log_file" 2>/dev/null; then
			return 0
		fi
		sleep 1
		((elapsed += 1))
	done
	return 1
}

cancel_active_codex() {
	local pid=$active_codex_pid
	if [[ -e "$active_cancel_requested_file" ]]; then
		return 0
	fi
	if [[ -z "$pid" ]] || ! process_alive "$pid"; then
		return 1
	fi
	: >"$active_cancel_requested_file"
	kill_tree "$pid" TERM
	local elapsed=0
	while process_alive "$pid" && ((elapsed < cancellation_shutdown_seconds)); do
		sleep 1
		((elapsed += 1))
	done
	if process_alive "$pid"; then
		kill_tree "$pid" KILL
	fi
	return 0
}

assert_events() {
	local scenario=$1
	local expression=$2
	local message=$3
	local events="$scenario_root/$scenario/events.jsonl"
	if ! jq -s -e "$expression" "$events" >/dev/null 2>&1; then
		fail_run "scenario $scenario $message"
	fi
}

assert_clean_repository() {
	if [[ "$(git -C "$codex_repo" rev-parse HEAD)" != "$initial_repo_commit" ]]; then
		fail_run "scenario $ACTIVE_SCENARIO changed the isolated repository history"
	fi
	if [[ -n "$(git -C "$codex_repo" status --porcelain=v1)" ]]; then
		fail_run "scenario $ACTIVE_SCENARIO changed files outside its declared behavior"
	fi
}

assert_apply_patch_result() {
	if [[ "$(git -C "$codex_repo" rev-parse HEAD)" != "$initial_repo_commit" ]]; then
		fail_run "scenario apply-patch changed the isolated repository history"
	fi
	if [[ ! -f "$codex_repo/smoke-target.txt" ]] || [[ $(<"$codex_repo/smoke-target.txt") != "after" ]]; then
		fail_run "scenario apply-patch did not produce the required file content"
	fi
	local status_line
	local status_seen=0
	while IFS= read -r status_line; do
		[[ -z "$status_line" ]] && continue
		status_seen=1
		case "$status_line" in
		" M smoke-target.txt"|"M  smoke-target.txt"|"MM smoke-target.txt") ;;
		*) fail_run "scenario apply-patch changed an unexpected repository path" ;;
		esac
	done < <(git -C "$codex_repo" status --porcelain=v1)
	((status_seen == 1)) || fail_run "scenario apply-patch left no observable working-tree edit"
}

run_scenario() {
	local scenario=$1
	local prompt
	local sandbox
	local events
	local codex_status
	prompt=$(scenario_prompt "$scenario")
	sandbox=$(scenario_sandbox "$scenario")
	ACTIVE_SCENARIO=$scenario
	start_codex_scenario "$scenario" "$prompt" "$sandbox"
	events="$scenario_root/$scenario/events.jsonl"

	if [[ "$scenario" == "cancel" ]]; then
		if ! wait_for_cancel_ready "$active_ready_file"; then
			fail_run "scenario cancel did not start streaming before its bounded arm timeout"
		fi
		if ! cancel_active_codex; then
			fail_run "scenario cancel completed before the suite could cancel the long-running request"
		fi
		finish_codex_scenario >/dev/null
		if ! curl --fail --silent --show-error --max-time 3 "$GATEWAY_URL/health/ready" | jq -e '.status == "ready"' >/dev/null; then
			fail_run "gateway was not healthy after scenario cancel"
		fi
		if ! wait_for_log_line "$gateway_log" 'canceled=true' "$cancellation_shutdown_seconds"; then
			fail_run "scenario cancel did not produce a canceled gateway request"
		fi
		assert_events "$scenario" '[.[] | select(.item_type == "agent_message" and (.phase == "started" or .phase == "delta"))] | length > 0' "did not expose a streamed assistant event before cancellation"
		assert_events "$scenario" '[.[] | select(.event_type == "turn.completed")] | length == 0' "completed before the client cancellation took effect"
		assert_clean_repository
		printf 'scenario passed: %s (canceled, %s structural events)\n' "$scenario" "$(wc -l <"$events" | tr -d ' ')"
		return 0
	fi

	if ! wait_for_status_file "$active_status_file" "$scenario_timeout_seconds"; then
		kill_tree "$active_codex_pid" TERM
		fail_run "scenario $scenario exceeded its ${scenario_timeout_seconds}s timeout"
	fi
	finish_codex_scenario
	codex_status=$CODEX_STATUS
	if [[ "$codex_status" != 0 ]]; then
		fail_run "scenario $scenario Codex exited unsuccessfully; model/provider behavior did not satisfy the scenario"
	fi
	assert_events "$scenario" '[.[] | select(.event_type == "turn.completed")] | length > 0' "did not complete a turn"
	case "$scenario" in
	text)
		assert_events "$scenario" '[.[] | select(.item_type == "agent_message")] | length > 0' "did not produce an assistant message event"
		assert_events "$scenario" '[.[] | select(.tool_event)] | length == 0' "used a tool in the text-only scenario"
		assert_clean_repository
		normal_scenario_count=$((normal_scenario_count + 1))
		;;
	inspect)
		assert_events "$scenario" '[.[] | select(.item_type == "command_execution" and .phase == "started")] | length == 1' "did not produce exactly one repository inspection tool event"
		assert_clean_repository
		normal_scenario_count=$((normal_scenario_count + 1))
		;;
	shell)
		assert_events "$scenario" '[.[] | select(.item_type == "command_execution" and .phase == "started")] | length == 1' "did not produce exactly one harmless shell tool event"
		assert_clean_repository
		normal_scenario_count=$((normal_scenario_count + 1))
		;;
	function)
		assert_events "$scenario" '[.[] | select(.tool_event and .phase == "started" and (.item_type == "function_call" or .item_type == "command_execution"))] | length == 1' "did not produce exactly one standard function/tool event"
		assert_clean_repository
		normal_scenario_count=$((normal_scenario_count + 1))
		;;
	apply-patch)
		assert_events "$scenario" '[.[] | select(.phase == "started" and (.item_type == "custom_tool_call" or .item_type == "apply_patch" or .item_type == "file_change"))] | length == 1' "did not expose exactly one apply_patch/file-change event"
		assert_apply_patch_result
		normal_scenario_count=$((normal_scenario_count + 1))
		;;
	parallel)
		assert_events "$scenario" '[.[] | select(.tool_event and .phase == "started")] | length >= 2' "did not attempt at least two tool calls"
		assert_events "$scenario" '
  [ .[] | select(.tool_event) ] as $tools |
  ($tools | map(.phase) | index("completed")) as $first_done |
  ($tools | map(select(.phase == "started")) | length >= 2) and
  ($first_done == null or ($tools[0:$first_done] | map(select(.phase == "started")) | length >= 2))
' "did not start two tools before the first tool completed"
		assert_clean_repository
		normal_scenario_count=$((normal_scenario_count + 1))
		;;
	esac
	printf 'scenario passed: %s (%s structural events)\n' "$scenario" "$(wc -l <"$events" | tr -d ' ')"
}

start_gateway
for scenario in "${selected_scenarios[@]}"; do
	run_scenario "$scenario"
done

if ((normal_scenario_count > 0)); then
	completed_responses=$(grep -Fc 'response_terminal=response.completed' "$gateway_log" || true)
	if ((completed_responses < normal_scenario_count)); then
		fail_run "gateway logs did not confirm a completed response for every non-cancellation scenario"
	fi
fi

if file_contains_forbidden "$gateway_log" || file_contains_forbidden "$gateway_stdout"; then
	fail_run "gateway diagnostics contained a credential, prompt marker, source path, or authorization value"
fi
if tree_contains_forbidden "$codex_home" || tree_contains_forbidden "$codex_repo"; then
	fail_run "Codex home or temporary repository contained a credential, prompt marker, or source path"
fi

suite_succeeded=1
echo "live scenario suite passed: ${selected_scenarios[*]}"
