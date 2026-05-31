#!/usr/bin/env bash
# -----------------------------------------------------------
# tests/e2e/cleanc/run_all.sh
#
# Drives every cleanc end-to-end scenario sequentially.
# Compose-less: cleanc is a single static binary with no
# database, queue, or HTTP gateway, so each scenario is a
# plain `bash run.sh` invocation with no docker-compose
# bring-up in front of it.
#
# Exits non-zero on the first failing scenario so CI can
# surface the failure without scrolling through subsequent
# scenarios' output. Honours CLEANC if exported (skips the
# per-scenario `go build`) so iterating locally is fast.
# -----------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_ROOT="$SCRIPT_DIR/scenarios"

if [[ ! -d "$SCENARIO_ROOT" ]]; then
    echo "run_all.sh: no scenarios/ directory at $SCENARIO_ROOT" >&2
    exit 2
fi

failed=()
ran=0
for scenario_dir in "$SCENARIO_ROOT"/*/; do
    scenario="$(basename "$scenario_dir")"
    runner="$scenario_dir/run.sh"
    if [[ ! -x "$runner" && ! -f "$runner" ]]; then
        echo "run_all.sh: skipping $scenario (no run.sh)"
        continue
    fi
    echo
    echo "===================================================================="
    echo "scenario: $scenario"
    echo "===================================================================="
    ran=$((ran + 1))
    if bash "$runner"; then
        echo "scenario PASS: $scenario"
    else
        echo "scenario FAIL: $scenario" >&2
        failed+=("$scenario")
        # Fail fast so CI logs stay readable.
        break
    fi
done

echo
if (( ${#failed[@]} > 0 )); then
    echo "run_all.sh: ${#failed[@]} scenario(s) failed: ${failed[*]}" >&2
    exit 1
fi
echo "run_all.sh: $ran scenario(s) passed."
