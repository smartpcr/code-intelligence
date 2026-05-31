# tests/e2e/cleanc

Shell-driven end-to-end golden tests for the `cleanc` CLI.

This directory mirrors the layout of the older
`tests/e2e/phase-09-audit-wal` harness so the CI scaffolding
plugs in naturally. Unlike the audit-WAL phase, `cleanc`
needs **no PostgreSQL, no HTTP gateway, and no docker stack**
-- it is a single static binary that walks a local repo path,
so each scenario can be exercised by a plain `bash run.sh`
with no `docker compose up` step in front of it.

## Layout

```text
tests/e2e/cleanc/
  README.md                    -- this file
  run_all.sh                   -- loops every scenarios/* and invokes its run.sh
  lib/
    normalize.jq               -- jq filter that masks non-deterministic UUIDs
                                   and ISO-8601 timestamps so the golden
                                   diff stays stable across runs
  scenarios/
    p0-go-cycle/
      repo/                    -- tarball-able sample Go repo with an
                                   import cycle between pkg/a and pkg/b
      golden/
        report.md              -- byte-match target (path-normalised)
        findings.json          -- byte-match target (UUID + timestamp masked)
        diag.json              -- byte-match target (already stable)
      run.sh                   -- builds cleanc, runs `analyze . --out report.md
                                   --findings findings.json --diagnostics diag.json`,
                                   normalises the outputs, diffs against golden/
    p0-mixed-langs/
      repo/                    -- one source file each for Go / Python /
                                   TypeScript / Java
      golden/
        expected-languages.txt -- the assertion target: the four canonical
                                   language tags that MUST show up in
                                   `RunArtifact.Files[].language`
      run.sh                   -- runs `cleanc analyze . --findings findings.json`,
                                   extracts the unique `Files[].language` set
                                   via jq, and asserts it equals the golden
```

## Running locally

```bash
# from repo root:
./tests/e2e/cleanc/run_all.sh

# or one scenario at a time:
./tests/e2e/cleanc/scenarios/p0-go-cycle/run.sh
```

`run_all.sh` aborts on the first failing scenario and exits
non-zero so CI can surface the failure without scrolling
through subsequent scenarios' output.

## Why the JSON outputs are normalised

The `cleanc` CLI mints random v4 UUIDs for `EvaluationRunID`,
`VerdictID`, `FindingID`, `HotSpotID`, `RefactorPlanID`, and
`RefactorTaskID`, and stamps `StartedAt` / `FinishedAt` /
`CreatedAt` / `DetectedAt` from the wall clock. None of that
is deterministic across runs, so a strict byte-for-byte diff
of the raw `findings.json` will always fail. The `lib/normalize.jq`
filter walks the JSON, replaces any string that matches the
canonical UUID shape with `<uuid>`, and any string that
matches an ISO-8601 timestamp with `<timestamp>`. The
`golden/findings.json` checked into this tree is already in
the normalised shape, and `run.sh` pipes the produced
artifact through the same filter before diffing.

`Context.RootPath` carries the absolute path the CLI was
invoked against, which is also runtime-dependent. `run.sh`
substitutes the actual absolute repo path with the synthetic
`/fixtures/<lang>` placeholder before the diff (matches the
synthetic root the in-tree godog goldens use under
`services/clean-code/internal/cli/testdata/golden`).

`diag.json` carries no UUIDs and no timestamps -- only
dark-metric counts and the effort-source tag -- so it is
byte-matched as-is.
