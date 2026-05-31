#!/usr/bin/env bash
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TMPDIR="$(mktemp -d)"
trap "rm -rf $TMPDIR" EXIT
echo 'package main' > "$TMPDIR/main.go"
"$CLEANC_BINARY_PATH" analyze "$TMPDIR"