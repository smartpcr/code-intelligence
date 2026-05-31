#!/usr/bin/env bash
TMPDIR="$(mktemp -d)"
trap "rm -rf $TMPDIR" EXIT
echo 'package main' > "$TMPDIR/main.go"
set +e
"$CLEANC_BINARY_PATH" analyze -telemetry-otlp http://localhost:4317 "$TMPDIR"
code=$?
set -e
exit "$code"