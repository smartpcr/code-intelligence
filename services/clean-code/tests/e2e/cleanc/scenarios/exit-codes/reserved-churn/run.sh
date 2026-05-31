#!/usr/bin/env bash
TMPDIR="$(mktemp -d)"
trap "rm -rf $TMPDIR" EXIT
echo 'package main' > "$TMPDIR/main.go"
set +e
"$CLEANC_BINARY_PATH" analyze -with-churn "$TMPDIR"
code=$?
set -e
exit "$code"