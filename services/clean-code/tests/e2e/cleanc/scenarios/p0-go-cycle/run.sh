#!/usr/bin/env bash
# p0-go-cycle: run the Go fixture through cleanc and capture artifacts.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../../.." && pwd)"

set +e
"$CLEANC_BINARY_PATH" analyze \
  -findings "$SCRIPT_DIR/findings.json" \
  -out "$SCRIPT_DIR/report.md" \
  "$REPO_ROOT/internal/cli/testdata/fixtures/go"
code=$?
set -e
exit "$code"