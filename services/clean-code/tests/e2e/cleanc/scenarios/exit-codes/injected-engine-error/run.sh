#!/usr/bin/env bash
TMPDIR="$(mktemp -d)"
POLICYDIR="$(mktemp -d)"
trap "rm -rf $TMPDIR $POLICYDIR" EXIT
echo 'package main' > "$TMPDIR/main.go"
echo 'THIS IS NOT YAML' > "$POLICYDIR/rules.yaml"
set +e
"$CLEANC_BINARY_PATH" analyze -policy "$POLICYDIR" "$TMPDIR"
code=$?
set -e
exit "$code"