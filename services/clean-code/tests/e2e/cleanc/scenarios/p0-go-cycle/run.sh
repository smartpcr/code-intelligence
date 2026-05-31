#!/usr/bin/env bash
# p0-go-cycle: run the Go fixture through cleanc, then normalize
# the report.md output for deterministic golden comparison.
#
# The binary produces non-deterministic UUIDs (uuid.NewV4),
# wall-clock timestamps, and machine-specific absolute paths.
# This script post-processes report.md so the committed golden
# can be byte-matched on any machine.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../../.." && pwd)"

# Resolve the module root as the binary normalises it (forward slashes).
if command -v cygpath >/dev/null 2>&1; then
  MODULE_ROOT=$(cygpath -m "$REPO_ROOT")
else
  MODULE_ROOT="$REPO_ROOT"
fi

set +e
"$CLEANC_BINARY_PATH" analyze \
  -findings "$SCRIPT_DIR/findings.json" \
  -out "$SCRIPT_DIR/report.md.raw" \
  "$REPO_ROOT/internal/cli/testdata/fixtures/go"
code=$?
set -e

# --- Normalize report.md for golden comparison ---
if [ -f "$SCRIPT_DIR/report.md.raw" ]; then
  # 1. Replace absolute module-root path and UUID patterns.
  sed -E \
    -e "s|${MODULE_ROOT}|/NORMALIZED_ROOT|g" \
    -e 's/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX/g' \
    "$SCRIPT_DIR/report.md.raw" > "$SCRIPT_DIR/report.md.tmp"

  # 2. Sort finding lines between "## Findings" and next "##".
  #    The engine sorts findings by random UUID so order varies.
  in_findings=false
  findings_file=$(mktemp)
  : > "$findings_file"
  {
    while IFS= read -r line || [ -n "$line" ]; do
      if [ "$line" = "## Findings" ]; then
        printf '%s\n' "$line"
        in_findings=true
      elif $in_findings; then
        case "$line" in
          "- "*)
            printf '%s\n' "$line" >> "$findings_file"
            ;;
          "## "*)
            LC_ALL=C sort "$findings_file"
            : > "$findings_file"
            in_findings=false
            printf '%s\n' "$line"
            ;;
          *)
            printf '%s\n' "$line"
            ;;
        esac
      else
        printf '%s\n' "$line"
      fi
    done < "$SCRIPT_DIR/report.md.tmp"
    if $in_findings; then
      LC_ALL=C sort "$findings_file"
    fi
  } > "$SCRIPT_DIR/report.md"
  rm -f "$SCRIPT_DIR/report.md.raw" "$SCRIPT_DIR/report.md.tmp" "$findings_file"
fi

exit "$code"