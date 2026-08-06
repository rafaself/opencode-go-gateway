#!/usr/bin/env bash

set -euo pipefail

duration=${FUZZTIME:-1s}
if [[ ! "$duration" =~ ^[0-9]+(ms|s|m)$ ]]; then
	echo "FUZZTIME must be a bounded Go duration such as 1s or 500ms" >&2
	exit 2
fi

run_fuzz() {
	local package=$1
	local target=$2
	echo "fuzz smoke: $package $target ($duration)"
	go test -run '^$' -fuzz "^${target}$" -fuzztime "$duration" "$package"
}

# Keep this list explicit: adding a fuzz target without adding it here would
# silently leave the target out of the bounded CI smoke pass.
run_fuzz ./internal/opencodego FuzzSSEDecoderChunkBoundaries
run_fuzz ./internal/opencodego FuzzProviderChunkJSON
run_fuzz ./internal/opencodego FuzzFragmentedToolArguments
run_fuzz ./internal/opencodego FuzzCustomToolArguments
run_fuzz ./internal/opencodego FuzzContinuationCorrelation
run_fuzz ./internal/codex FuzzResponsesRequestDecoder
run_fuzz ./internal/server FuzzErrorSerialization

echo "bounded fuzz smoke passed"
