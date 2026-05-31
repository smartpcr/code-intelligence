#!/usr/bin/env bash
set +e
"$CLEANC_BINARY_PATH" analyze /nonexistent/path/does/not/exist
code=$?
set -e
exit "$code"