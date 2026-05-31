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
| `scan`              | not implemented   | Scan one repository (local path or git URL). |
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
