#!/usr/bin/env bash
# -----------------------------------------------------------
# tests/e2e/cleanc/scenarios/p0-mixed-langs/run.sh
#
# End-to-end coverage check that the L1 walker + L2 parser
# fleet light up for all four supported languages
# (Go / Python / TypeScript / Java) in a single run.
#
# Pipeline:
#   1. Build cleanc (skipped if $CLEANC is set).
#   2. Run `cleanc analyze . --findings findings.json` against
#      ./repo, which contains one source file per language.
#   3. Extract the unique sorted `Files[].language` set from
#      the produced findings.json via jq.
#   4. Diff that set against golden/expected-languages.txt;
#      the assertion fails if any of {go, java, python,
#      typescript} are missing from `RunArtifact.Files`.
# -----------------------------------------------------------
set -euo pipefail

SCENARIO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_ROOT="$(cd "$SCENARIO_DIR/../.." && pwd)"
REPO_ROOT="$(cd "$E2E_ROOT/../../.." && pwd)"
SAMPLE_REPO="$SCENARIO_DIR/repo"
GOLDEN_DIR="$SCENARIO_DIR/golden"

OUT_DIR="$(mktemp -d -t cleanc-p0-mixed-langs-XXXXXX)"
trap 'rm -rf "$OUT_DIR"' EXIT

# ---------------------------------------------------------------
# Step 1: build cleanc.
# ---------------------------------------------------------------
if [[ -z "${CLEANC:-}" ]]; then
    CLEANC="$OUT_DIR/cleanc"
    echo "[p0-mixed-langs] building cleanc -> $CLEANC"
    (cd "$REPO_ROOT/services/clean-code" && go build -o "$CLEANC" ./cmd/cleanc)
fi

# ---------------------------------------------------------------
# Step 2: run analyze. We do NOT request --out / --diagnostics
#          because the assertion only needs findings.json's
#          Files[].language column.
# ---------------------------------------------------------------
echo "[p0-mixed-langs] running cleanc analyze . (in $SAMPLE_REPO)"
set +e
(
    cd "$SAMPLE_REPO"
    "$CLEANC" analyze . \
        --out "$OUT_DIR/report.md" \
        --findings "$OUT_DIR/findings.json" \
        >"$OUT_DIR/stdout.txt" 2>"$OUT_DIR/stderr.txt"
)
exit_code=$?
set -e

# This fixture has no policy-blocking violations, so cleanc
# may exit 0 (clean) or 1 (warn/info-only finding). Any other
# code is an internal failure.
if [[ $exit_code -ne 0 && $exit_code -ne 1 ]]; then
    echo "[p0-mixed-langs] cleanc analyze failed with exit $exit_code" >&2
    echo "----- stderr -----" >&2
    cat "$OUT_DIR/stderr.txt" >&2
    exit "$exit_code"
fi

if [[ ! -s "$OUT_DIR/findings.json" ]]; then
    echo "[p0-mixed-langs] cleanc did not produce findings.json" >&2
    exit 2
fi

# ---------------------------------------------------------------
# Step 3: extract the unique sorted language set from
#          RunArtifact.Files[].language. Empty language tags
#          (skipped files) are filtered out so the assertion
#          only checks parsed-file coverage.
# ---------------------------------------------------------------
jq -r '[.Files[]?.language] | map(select(. != null and . != "")) | unique | .[]' \
    "$OUT_DIR/findings.json" > "$OUT_DIR/got-languages.txt"

# ---------------------------------------------------------------
# Step 4: assert.
# ---------------------------------------------------------------
echo "[p0-mixed-langs] diffing detected vs expected languages"
echo "----- expected -----"
cat "$GOLDEN_DIR/expected-languages.txt"
echo "----- got -----"
cat "$OUT_DIR/got-languages.txt"
diff -u "$GOLDEN_DIR/expected-languages.txt" "$OUT_DIR/got-languages.txt"

echo "[p0-mixed-langs] PASS"
