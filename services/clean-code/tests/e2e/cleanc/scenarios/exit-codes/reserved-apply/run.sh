#!/usr/bin/env bash
set +e
"$CLEANC_BINARY_PATH" apply /tmp
code=$?
set -e
exit "$code"