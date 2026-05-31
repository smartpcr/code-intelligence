#!/usr/bin/env bash
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../../../.." && pwd)"
set +e
"$CLEANC_BINARY_PATH" analyze "$REPO_ROOT/internal/cli/testdata/fixtures/go"
code=$?
set -e
exit "$code"