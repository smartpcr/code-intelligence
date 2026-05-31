#!/usr/bin/env bash
# p0-go-cycle: run the Go fixture through cleanc and capture artifacts.
# CLEANC_DISPLAY_ROOT makes the binary emit a stable display path
# instead of the machine-specific absolute path, so the raw report.md
# is byte-identical across machines and can be golden-matched.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../../.." && pwd)"

# Export separately to avoid MSYS path conversion on the value.
export CLEANC_DISPLAY_ROOT="REPO_ROOT"

set +e
"$CLEANC_BINARY_PATH" analyze \
  -findings "$SCRIPT_DIR/findings.json" \
  -out "$SCRIPT_DIR/report.md" \
  "$REPO_ROOT/internal/cli/testdata/fixtures/go"
code=$?
set -e
exit "$code"