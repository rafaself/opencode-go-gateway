#!/usr/bin/env bash

set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
scenario_script="$script_dir/live-scenarios.sh"
structural_filter="$script_dir/live-scenarios-structural.jq"

bash -n "$scenario_script"
"$scenario_script" --validate --all >/dev/null
[[ "$("$scenario_script" --list | wc -l | tr -d ' ')" == 7 ]]

set +e
opt_in_output=$(RUN_LIVE_SCENARIOS=0 OPENCODE_GO_API_KEY=offline-test-key "$scenario_script" text 2>&1)
opt_in_status=$?
set -e
[[ "$opt_in_status" == 2 ]]
grep -Fq 'opt-in' <<<"$opt_in_output"

set +e
invalid_output=$("$scenario_script" --validate not-a-scenario 2>&1)
invalid_status=$?
set -e
[[ "$invalid_status" == 2 ]]
grep -Fq 'unknown scenario' <<<"$invalid_output"

set +e
all_combination_output=$("$scenario_script" --validate --all text 2>&1)
all_combination_status=$?
set -e
[[ "$all_combination_status" == 2 ]]
grep -Fq -- '--all cannot be combined' <<<"$all_combination_output"

grep -Fq 'RUN_LIVE_SCENARIOS=1' "$scenario_script"
grep -Fq 'env -u OPENCODE_GO_API_KEY' "$scenario_script"
grep -Fq 'OPENCODE_GATEWAY_PORT=0' "$scenario_script"
grep -Fq 'setup codex --codex-home' "$scenario_script"
grep -Fq 'model_catalog_json=' "$scenario_script"
grep -Fq -- '--ignore-user-config' "$scenario_script"
grep -Fq -- '--ephemeral' "$scenario_script"
grep -Fq 'mkfifo' "$scenario_script"
grep -Fq 'response_terminal=response.completed' "$scenario_script"
grep -Fq 'tree_contains_forbidden' "$scenario_script"

structural_output=$(printf '%s\n' \
	'{"type":"item.completed","item":{"type":"agent_message","text":"prompt-secret","id":"/unsafe/source"},"delta":"prompt-secret"}' \
	'{"type":"item.started","item":{"type":"command_execution","command":"cat /unsafe/source","status":"prompt-secret"}}' \
	'{"type":"turn.completed","usage":{"input_tokens":999}}' |
	jq --unbuffered -c -S -f "$structural_filter")
if grep -Fq 'prompt-secret' <<<"$structural_output" ||
	grep -Fq '/unsafe/source' <<<"$structural_output" ||
	grep -Fq 'input_tokens' <<<"$structural_output" ||
	grep -Fq '"command":' <<<"$structural_output"; then
	echo "structural filter retained sensitive event content" >&2
	exit 1
fi

echo "live scenario offline/static tests passed"
