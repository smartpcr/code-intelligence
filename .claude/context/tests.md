# AST Parser Test Documentation

## Supported Language Matrix

| Language | CGO=on | CGO=off | Notes |
|----------|--------|---------|-------|
| TypeScript | tree-sitter | scanner | Baseline; fully supported |
| Go | tree-sitter | skip | Requires CGO for tree-sitter binding |
| C | tree-sitter | skip | Requires CGO for tree-sitter binding |
| C++ | tree-sitter | skip | Requires CGO for tree-sitter binding |
| C# | tree-sitter | skip | Requires CGO for tree-sitter binding |
| Rust | tree-sitter | skip | Requires CGO for tree-sitter binding |
| PowerShell | subprocess | subprocess | Uses pwsh subprocess; no tree-sitter grammar |

## Skip Keys

- `no_parser` — used when CGO is disabled and the language parser requires tree-sitter (CGO-only).
- `pwsh_not_available` — used when `pwsh` is not found on `$PATH`; PowerShell fixture tests skip gracefully.

## Notes

- Tree-sitter-backed support requires CGO (`CGO_ENABLED=1`).
- Non-CGO scanner support may be narrower; only TypeScript has a scanner fallback.
- PowerShell requires additional grammar work unless scanner-only; currently uses subprocess invocation.