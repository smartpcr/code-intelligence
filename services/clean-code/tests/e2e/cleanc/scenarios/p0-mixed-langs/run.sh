#!/usr/bin/env bash
# p0-mixed-langs: run a 4-language corpus through cleanc.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

set +e
"$CLEANC_BINARY_PATH" analyze \
  -findings "$SCRIPT_DIR/findings.json" \
  "$SCRIPT_DIR/corpus"
code=$?
set -e
exit "$code"