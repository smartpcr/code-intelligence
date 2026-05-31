# Changelog — code-intelligence (REFACTOR-GUIDE)

All notable changes to the `cleanc` CLI and its end-to-end
test harness land in this file. The format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
because this repo is still pre-1.0 and the cleanc CLI is
shipped under an evolving story spec, version numbering
mirrors the `REFACTOR-GUIDE` phase / stage pins rather than
SemVer.

## Unreleased

### Added

- **End-to-end golden test harness** at `tests/e2e/cleanc/`.
  Shell-driven, compose-less (single static binary, no
  PostgreSQL / HTTP / docker stack). Each scenario is a
  self-contained directory with a `repo/` (tarball-able
  sample input), a `run.sh` driver, and a `golden/`
  directory of expected artifacts. Two P0 scenarios shipped:
  - `scenarios/p0-go-cycle/` — Go module with a `pkg/a ↔
    pkg/b` import cycle; asserts byte-match of `report.md`,
    `findings.json`, and `diag.json` against checked-in
    golden after path-normalisation, UUID/timestamp masking,
    and stable-order canonicalisation.
  - `scenarios/p0-mixed-langs/` — one source file each of
    Go / Python / TypeScript / Java; asserts all four
    languages appear in `RunArtifact.Files`.
- `tests/e2e/cleanc/lib/normalize.jq` — jq filter that masks
  random v4 UUIDs to `"<uuid>"`, ISO-8601 timestamps to
  `"<timestamp>"`, and canonicalises array order via
  `sort_by(tojson)` so the byte-match diff is stable across
  runs.
- `tests/e2e/cleanc/lib/normalize-md.sh` — awk-based markdown
  bullet sorter so the engine's non-deterministic insertion
  order doesn't bleed into the report.md diff.
- `tests/e2e/cleanc/run_all.sh` — sequential scenario driver
  that exits non-zero on the first failure so CI logs stay
  readable.
- `docs/cleanc/PROMPT-FORMAT.md` — draft schema doc for the
  `--emit-prompts` JSONL stream emitted by the P1
  structured-prompt-emitter stages.

### Changed

- `services/clean-code/README.md` and `docs/cleanc/USAGE.md`
  now document the end-to-end golden harness and how to run
  it locally (`./tests/e2e/cleanc/run_all.sh`).
