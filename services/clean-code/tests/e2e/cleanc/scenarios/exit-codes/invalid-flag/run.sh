#!/usr/bin/env bash
set +e
"$CLEANC_BINARY_PATH" analyze --no-such-flag /tmp
code=$?
set -e
exit "$code"