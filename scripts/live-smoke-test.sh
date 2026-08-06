#!/usr/bin/env bash

set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
assistant_output_filter="$script_dir/live-smoke-assistant-output.jq"

if ! command -v jq >/dev/null 2>&1; then
	echo "required command not found: jq" >&2
	exit 2
fi

assert_output() {
	local event=$1
	if ! printf '%s\n' "$event" | jq -e -f "$assistant_output_filter" >/dev/null; then
		echo "expected assistant output event to match: $event" >&2
		exit 1
	fi
}

assert_not_output() {
	local event=$1
	if printf '%s\n' "$event" | jq -e -f "$assistant_output_filter" >/dev/null; then
		echo "expected non-output event not to match: $event" >&2
		exit 1
	fi
}

assert_output '{"type":"item.started","item":{"type":"agent_message","text":"hello"}}'
assert_output '{"type":"item.updated","item":{"type":"agent_message","text":"hello"}}'
assert_output '{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}'
assert_output '{"type":"item.delta","item":{"type":"agent_message"},"delta":"hello"}'
assert_output '[{"type":"thread.started"},{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}]'

assert_not_output '{"type":"thread.started","thread_id":"thread-1"}'
assert_not_output '{"type":"turn.started"}'
assert_not_output '{"type":"item.started","item":{"type":"agent_message","text":""}}'
assert_not_output '{"type":"item.updated","item":{"type":"reasoning","text":"internal"}}'
assert_not_output '{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"hello"}}'
assert_not_output '[{"type":"thread.started"},{"type":"turn.completed"}]'

echo "live smoke assistant-output filter tests passed"
