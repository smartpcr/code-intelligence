# codeintel

`codeintel` is the developer-facing command-line driver for the
`scan-repo` capability described in §3.1 of the REPO-SCANNER
[architecture](../../../../docs/stories/code-intelligence-REPO-SCANNER/architecture.md).
It wraps the existing AST dispatcher
and graphsink/materializer abstractions so a developer can scan
external repositories — by local path or by `git URL @ sha` —
and project module/call-chain diagrams without standing up the
full production stack (Postgres + Qdrant + OTel).

## Subcommands (scaffolded; real bodies land in stages 5.2 – 5.5)

| Command             | Status            | Purpose |
| ------------------- | ----------------- | ------- |
| `scan`              | working           | Scan one repository (local path or git URL). |
| `scan-many`         | not implemented   | Iterate a manifest of `<path>` / `<url>@<sha>` lines. |
| `diagram module`    | not implemented   | Project the top-down module/component diagram. |
| `diagram calls`     | not implemented   | Project the left-right call-chain diagram from a seed. |
| `serve`             | not implemented   | Serve diagram JSON over HTTP for the React UI. |
| `version`           | working           | Print build metadata (`-X main.version=…`). |

## Persistent flags

| Flag                | Default  | Description |
| ------------------- | -------- | ----------- |
| `--store`           | `sqlite` | Graph store backend: `sqlite`, `postgres`, `memory`. |
| `--db`              | _empty_  | Store connection string or file path (sqlite: file path; postgres: DSN; memory: ignored). |
| `--log`             | `text`   | slog handler format: `text` or `json`. |
| `--with-embeddings` | `false`  | Opt in to the embedding publisher (requires the recall stack). |

`--log=json` wires `slog.Default` to `slog.NewJSONHandler` so all
log lines are structured JSON parseable by `encoding/json`.

## Build

```powershell
cd services\agent-memory
go build -ldflags "-X main.version=v0.1.0 -X main.commit=$(git rev-parse --short HEAD) -X main.buildDate=$(Get-Date -Format o)" .\cmd\codeintel
```

`CGO_ENABLED=1` is required for full language coverage; a CGO=0
build degrades to PowerShell-only on `.c/.cpp/.cs/.go/.rs` files
per tech-spec §C7.

## `scan` subcommand

Drive the AST dispatcher over one repository synchronously
(no queue, no Postgres/Qdrant required at the default
`--store=sqlite`).

### Synopsis

```powershell
codeintel scan <path-or-url> [--sha <sha>] [--out <path>] [--lang-hints <list>]
```

The first positional argument is classified as:

| Input form              | Materializer            | `--sha` required? |
| ----------------------- | ----------------------- | ----------------- |
| Absolute local path     | `LocalDirMaterializer`  | No (synthesised)  |
| `file://...` URL        | `LocalDirMaterializer`  | No                |
| `https://`, `git://`, `ssh://`, `git+ssh://`, `git+https://`, `git@host:org/repo` | `GitMaterializer` | **Yes** |

### `scan`-specific flags

| Flag           | Default                                    | Description |
| -------------- | ------------------------------------------ | ----------- |
| `--sha`        | _empty_                                    | Override the repository SHA. **Required** for git URLs. For local inputs, overrides the synthesised SHA (`git rev-parse HEAD` or the mtime-tree hash). |
| `--out`        | _empty_                                    | Output path. For `--store=sqlite` the `.db` file path; for `--store=memory` the JSON export file path (omit to skip the export). If neither `--out` nor `--db` is set and `--store=sqlite`, a path is synthesised from the input (see the `scan.default_sqlite_path` log line). |
| `--lang-hints` | _empty_                                    | Comma-separated language hints (e.g. `--lang-hints=python,typescript`). Forwarded to `AncestryWriter.EnsureRepo` and used as a tie-breaker for files whose extension does not map to a registered parser. |

### Examples

Scan a local checkout with all defaults (writes `./codeintel-<basename>.db`):

```powershell
codeintel scan E:\src\my-repo
```

Scan a git URL at a specific commit and write a SQLite store:

```powershell
codeintel scan https://github.com/example/repo.git --sha 1234567890abcdef --out repo.db
```

Scan to memory and export a JSON graph for the diagram UI:

```powershell
codeintel --store memory scan E:\src\my-repo --out graph.json
```

Restrict the dispatcher to a hint list of languages:

```powershell
codeintel scan E:\src\my-repo --lang-hints python,typescript
```

### Summary output

On completion `scan` prints a structured summary to stdout. In
`--log=text` mode (default) it is human-readable:

```text
repo: file:///E:/src/my-repo (sha=local) node-id=...
walked: 142
parsed: 138
nodes: repo=1 package=4 file=138 class=12 method=87
edges: contains=137 imports=24 static_calls=51
skipped: by_reason{no_parser:4} no_parser_by_ext{.zzz:4}
```

In `--log=json` mode the last line of stdout is a single JSON
document matching `scanSummary`:

```json
{"repo":{"url":"file:///E:/src/my-repo","sha":"local","node_id":"..."},
 "walked":142,"parsed":138,
 "nodes":{"repo":1,"package":4,"file":138,"class":12,"method":87},
 "edges":{"contains":137,"imports":24,"static_calls":51},
 "skipped":{"by_reason":{"no_parser":4},"no_parser_by_ext":{".zzz":4}}}
```

`parsed` excludes files the dispatcher classified as skipped
(`no_parser` for unsupported extensions or CGO=0 builds;
`pwsh_not_available` when `pwsh` is missing). `nodes` / `edges`
report the resulting scan graph (unique IDs), so a re-scan of an
unchanged repo reports the same shape rather than zeros.

Coverage-degraded scans (e.g. CGO=0 skipping `.c/.cpp/.go/.rs`)
exit 0; only fatal IO/config errors yield a non-zero exit code.
A failed `--out` export (e.g. memory backend writing to an
unwritable path) is treated as fatal — `sink close` errors are
surfaced as the scan's overall result.
