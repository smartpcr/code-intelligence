#!/usr/bin/env bash
# p1-prompts: run Go fixture with --emit-prompts.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../../.." && pwd)"

set +e
"$CLEANC_BINARY_PATH" analyze \
  -findings "$SCRIPT_DIR/findings.json" \
  -emit-prompts "$SCRIPT_DIR/prompts.jsonl" \
  "$REPO_ROOT/internal/cli/testdata/fixtures/go"
code=$?
set -e
exit "$code"