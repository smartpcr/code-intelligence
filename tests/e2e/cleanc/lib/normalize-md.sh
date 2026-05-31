#!/usr/bin/env bash
# -----------------------------------------------------------
# tests/e2e/cleanc/lib/normalize-md.sh
#
# Markdown report normaliser. The cleanc markdown renderer
# emits each section's bullet list in the engine's insertion
# order, which is not deterministic across runs (random v4
# UUIDs drive the row ordering inside `EvaluationVerdict` /
# `Finding` / `HotSpot`).  We canonicalise the report by
# alphabetically sorting any contiguous run of `- ` bullet
# lines.  All other lines (headers, key-value pairs, blank
# lines) are passed through unchanged.
#
# Usage: normalize-md.sh <markdown-file>
#        Rewrites the file in place.
# -----------------------------------------------------------
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "normalize-md.sh: expected exactly 1 argument (file path)" >&2
    exit 64
fi

target="$1"

# awk reads the file, buffers any contiguous block of `- `
# bullet lines, sorts the buffer with `sort` over a pipe,
# and re-emits in alpha order. Non-bullet lines flush the
# buffer first, then print themselves.
awk '
function flush(    i, n) {
    if (count == 0) return
    cmd = "sort"
    for (i = 1; i <= count; i++) print buf[i] | cmd
    close(cmd)
    for (i = 1; i <= count; i++) delete buf[i]
    count = 0
}
BEGIN { count = 0 }
{
    if ($0 ~ /^- /) {
        buf[++count] = $0
    } else {
        flush()
        print
    }
}
END { flush() }
' "$target" > "$target.tmp"
mv "$target.tmp" "$target"
