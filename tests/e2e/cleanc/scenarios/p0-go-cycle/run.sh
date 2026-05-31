#!/usr/bin/env bash
# -----------------------------------------------------------
# tests/e2e/cleanc/scenarios/p0-go-cycle/run.sh
#
# End-to-end golden test for the P0 "Go import cycle" scenario.
#
# Pipeline:
#   1. Build the `cleanc` binary (skipped if $CLEANC is set).
#   2. Run `cleanc analyze . --out report.md --findings findings.json
#        --diagnostics diag.json` against ./repo.
#   3. Substitute the runtime-dependent absolute repo path
#      with the synthetic `/fixtures/go` placeholder.
#   4. Pipe findings.json through lib/normalize.jq to mask
#      non-deterministic UUIDs and ISO-8601 timestamps.
#   5. Byte-diff the produced report.md, findings.json, and
#      diag.json against the files checked into ./golden.
# -----------------------------------------------------------
set -euo pipefail

SCENARIO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_ROOT="$(cd "$SCENARIO_DIR/../.." && pwd)"
REPO_ROOT="$(cd "$E2E_ROOT/../../.." && pwd)"
SAMPLE_REPO="$SCENARIO_DIR/repo"
GOLDEN_DIR="$SCENARIO_DIR/golden"
NORMALIZER="$E2E_ROOT/lib/normalize.jq"
SYNTHETIC_PATH="/fixtures/go"

OUT_DIR="$(mktemp -d -t cleanc-p0-go-cycle-XXXXXX)"
trap 'rm -rf "$OUT_DIR"' EXIT

# ---------------------------------------------------------------
# Step 1: build cleanc (skip if caller passed a pre-built binary).
# ---------------------------------------------------------------
if [[ -z "${CLEANC:-}" ]]; then
    CLEANC="$OUT_DIR/cleanc"
    echo "[p0-go-cycle] building cleanc -> $CLEANC"
    (cd "$REPO_ROOT/services/clean-code" && go build -o "$CLEANC" ./cmd/cleanc)
fi

# ---------------------------------------------------------------
# Step 2: run analyze against the sample repo.
# ---------------------------------------------------------------
ABS_REPO="$(cd "$SAMPLE_REPO" && pwd)"
echo "[p0-go-cycle] running cleanc analyze . (in $ABS_REPO)"
set +e
(
    cd "$SAMPLE_REPO"
    "$CLEANC" analyze . \
        --out "$OUT_DIR/report.md" \
        --findings "$OUT_DIR/findings.json" \
        --diagnostics "$OUT_DIR/diag.json" \
        >"$OUT_DIR/stdout.txt" 2>"$OUT_DIR/stderr.txt"
)
exit_code=$?
set -e

# Verdict is `block` for this fixture, so cleanc exits with the
# finding-triggered exit code (1) by default. Any code other
# than 0 (clean) or 1 (finding-triggered) is a CLI/internal
# failure and must surface.
if [[ $exit_code -ne 0 && $exit_code -ne 1 ]]; then
    echo "[p0-go-cycle] cleanc analyze failed with exit $exit_code" >&2
    echo "----- stderr -----" >&2
    cat "$OUT_DIR/stderr.txt" >&2
    exit "$exit_code"
fi

for f in report.md findings.json diag.json; do
    if [[ ! -s "$OUT_DIR/$f" ]]; then
        echo "[p0-go-cycle] cleanc did not produce $f" >&2
        exit 2
    fi
done

# ---------------------------------------------------------------
# Step 3: substitute the absolute repo path with the synthetic
#          placeholder so report.md / findings.json can byte-match
#          a golden whose Context.RootPath is `/fixtures/go`.
# ---------------------------------------------------------------
# sed -i syntax differs between BSD (macOS) and GNU. Use a temp
# file so the script is portable across both.
sed_in_place () {
    local file="$1"
    sed -e "s|$ABS_REPO|$SYNTHETIC_PATH|g" "$file" > "$file.tmp"
    mv "$file.tmp" "$file"
}
sed_in_place "$OUT_DIR/report.md"
sed_in_place "$OUT_DIR/findings.json"

# ---------------------------------------------------------------
# Step 4: normalise findings.json (mask UUIDs + timestamps).
# ---------------------------------------------------------------
jq -f "$NORMALIZER" "$OUT_DIR/findings.json" > "$OUT_DIR/findings.normalised.json"
mv "$OUT_DIR/findings.normalised.json" "$OUT_DIR/findings.json"

# ---------------------------------------------------------------
# Step 4b: canonicalise report.md bullet ordering. The markdown
#          renderer emits findings / dark-metric bullets in the
#          engine's insertion order, which is non-deterministic
#          across runs.  normalize-md.sh sorts any contiguous
#          block of `- ` bullets alphabetically; both the
#          produced report and the checked-in golden are passed
#          through the same filter so the diff is stable.
# ---------------------------------------------------------------
"$E2E_ROOT/lib/normalize-md.sh" "$OUT_DIR/report.md"

# ---------------------------------------------------------------
# Step 5: byte-diff against golden.
# ---------------------------------------------------------------
echo "[p0-go-cycle] diffing against $GOLDEN_DIR"
diff -u "$GOLDEN_DIR/report.md"     "$OUT_DIR/report.md"
diff -u "$GOLDEN_DIR/findings.json" "$OUT_DIR/findings.json"
diff -u "$GOLDEN_DIR/diag.json"     "$OUT_DIR/diag.json"

echo "[p0-go-cycle] PASS"
