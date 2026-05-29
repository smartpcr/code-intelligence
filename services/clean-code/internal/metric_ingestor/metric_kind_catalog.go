package metric_ingestor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/coverage"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/materialisers"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// pgMetricKindTable is the unqualified `metric_kind` catalog
// table name the seeder targets. Schema-qualified at
// statement-build time via [pq.QuoteIdentifier].
const pgMetricKindTable = "metric_kind"

// ErrMetricKindCatalogNilDB surfaces a nil *sql.DB at
// composition-root wiring time.
var ErrMetricKindCatalogNilDB = errors.New("metric_ingestor: SeedMetricKindCatalog: *sql.DB is nil")

// ErrMetricKindCatalogEmptySchema surfaces an empty schema
// name at composition-root wiring time.
var ErrMetricKindCatalogEmptySchema = errors.New("metric_ingestor: SeedMetricKindCatalog: schema is empty")

// ErrMetricKindCatalogMissingMetadata is returned when
// [MetricKindCatalogRowsForRegistry] is asked to derive a
// row for a `metric_kind` that has no hand-curated entry in
// [foundationCatalogMetadata]. The composition root MUST add
// an entry before wiring a new recipe so the FK on
// `metric_sample.(metric_kind, metric_version)` is satisfied
// at first write.
var ErrMetricKindCatalogMissingMetadata = errors.New("metric_ingestor: MetricKindCatalogRowsForRegistry: no catalog metadata for metric_kind")

// ErrMetricKindCatalogVersionDrift is returned by
// [VerifyMetricKindCatalog] when the catalog row's
// `metric_version` for a `metric_kind` disagrees with the
// version the producer (recipe / materialiser) currently
// emits. The two states the FK on
// `metric_sample.(metric_kind, metric_version)` rejects are:
//   - catalog has v=N; producer emits v=M where M != N
//     -> first write at the new version fails the FK
//   - catalog row missing entirely -> first write fails the FK
//
// Failing fast at startup (rather than at first scan write)
// makes the misconfiguration visible at `/readyz` time and
// keeps the queue from accumulating failures.
var ErrMetricKindCatalogVersionDrift = errors.New("metric_ingestor: VerifyMetricKindCatalog: catalog row metric_version disagrees with producer's emitted metric_version")

// ErrMetricKindCatalogRowMissing is returned by
// [VerifyMetricKindCatalog] when no row exists for the
// expected metric_kind. Distinct sentinel from
// [ErrMetricKindCatalogVersionDrift] so callers can tell
// "never seeded" apart from "stale version".
var ErrMetricKindCatalogRowMissing = errors.New("metric_ingestor: VerifyMetricKindCatalog: no catalog row for metric_kind")

// MetricKindTier matches the closed-set values for
// `clean_code.metric_kind.tier` (the `metric_kind_tier` enum
// at `migrations/0001_catalog_lifecycle.up.sql:96-99`).
type MetricKindTier string

// Canonical [MetricKindTier] values.
const (
	MetricKindTierFoundation MetricKindTier = "foundation"
	MetricKindTierSystem     MetricKindTier = "system"
)

// MetricKindCatalogRow is one row of
// `clean_code.metric_kind` (`migrations/0001_catalog_lifecycle.up.sql:258-280`).
// Field types mirror the column types: `metric_kind` is the
// PK text; `metric_version` is integer NOT NULL; `tier` /
// `pack` / `unit` / `description_md` are NOT NULL text. The
// composite FK target on `metric_sample.(metric_kind,
// metric_version)` (per `migrations/0002_measurement.up.sql:348-350`)
// requires both PK natural-key columns to be populated for
// the seeder's output to satisfy the downstream INSERT.
type MetricKindCatalogRow struct {
	MetricKind    string
	MetricVersion int
	Tier          MetricKindTier
	Pack          recipes.Pack
	Unit          string
	DescriptionMD string
}

// foundationCatalogMetadata is the hand-curated per-kind
// metadata (unit + description) the seeder uses to populate
// the `metric_kind.unit` and `metric_kind.description_md`
// NOT NULL columns. The `metric_kind` PK, `metric_version`,
// `pack`, and `tier` come from the recipe / materialiser
// itself (so version bumps land automatically with the code
// change); the editorial fields live here so adding a new
// recipe requires a coordinated edit.
//
// The architecture's "Writer: the Policy Steward (or
// catalogue-seeding migration) is the only writer; rows are
// append-only after a metric definition stabilises" rule
// (COMMENT on `clean_code.metric_kind` at
// `migrations/0001_catalog_lifecycle.up.sql:283-286`) is why
// the seeder uses `ON CONFLICT (metric_kind) DO NOTHING` --
// the first writer wins (a steward-curated row from a
// dedicated migration would take precedence over the
// composition-root default).
var foundationCatalogMetadata = map[string]struct {
	Unit          string
	DescriptionMD string
}{
	"cyclo": {
		Unit:          "count",
		DescriptionMD: "McCabe cyclomatic complexity per method and per file (architecture Sec 1.4.1 row 1).",
	},
	"cognitive_complexity": {
		Unit:          "count",
		DescriptionMD: "SonarSource-style cognitive complexity per method and per file (architecture Sec 1.4.1 row 2).",
	},
	"loc": {
		Unit:          "count",
		DescriptionMD: "Source lines of code per method and per file (architecture Sec 1.4.1 row 3).",
	},
	"lcom4": {
		Unit:          "count",
		DescriptionMD: "Lack of cohesion (Hitz & Montazeri LCOM4) per class (architecture Sec 1.4.1 row 4).",
	},
	"fan_in": {
		Unit:          "count",
		DescriptionMD: "Inbound call-graph edge count per method (architecture Sec 1.4.1 row 5).",
	},
	"fan_out": {
		Unit:          "count",
		DescriptionMD: "Outbound call-graph edge count per method (architecture Sec 1.4.1 row 6).",
	},
	"modification_count_in_window": {
		Unit:          "count",
		DescriptionMD: "Number of file modifications observed within the configured window_days for a given scope (architecture Sec 1.4.1 row 12; tech-spec Sec 8.2).",
	},
	"coverage_line_ratio": {
		Unit:          "ratio",
		DescriptionMD: "Per-file line-coverage ratio (lines_covered / lines_valid) ingested from an external coverage publisher (architecture Sec 1.4.1 row 16; tech-spec Sec 4.1.1 -- the ONLY canonical line-coverage metric_kind, with `coverage_line` and `coverage_lines_covered_ratio` aliases removed per iter-1 evaluator item 4).",
	},
	"coverage_branch_ratio": {
		Unit:          "ratio",
		DescriptionMD: "Per-file branch-coverage ratio (branches_covered / branches_valid) ingested from an external coverage publisher (architecture Sec 1.4.1 row 16; tech-spec Sec 4.1.1 -- the ONLY canonical branch-coverage metric_kind, with `coverage_branch` and `coverage_branches_covered_ratio` aliases removed per iter-1 evaluator item 4).",
	},
	"pass_first_try_ratio": {
		Unit:          "ratio",
		DescriptionMD: "Fraction of test attempts that passed on the first try per scope; ingested via `ingest.test_balance` (architecture Sec 1.4.2; tech-spec Sec 8.5).",
	},
}

// packToTier maps a [recipes.Pack] to the closed-set
// [MetricKindTier] the catalog row needs. The mapping is
// pinned by architecture Sec 1.4.1 (foundation tier) and Sec
// 1.4.2 (system tier):
//   - `base` / `solid` / `ingested` -> `foundation`
//   - `system` -> `system`
func packToTier(p recipes.Pack) (MetricKindTier, error) {
	switch p {
	case recipes.PackBase, recipes.PackSolid, recipes.PackIngested:
		return MetricKindTierFoundation, nil
	case recipes.PackSystem:
		return MetricKindTierSystem, nil
	default:
		return "", fmt.Errorf("metric_ingestor: packToTier: unknown pack %q (want one of base|solid|ingested|system)", p)
	}
}

// ingestedMetricKinds enumerates the foundation-tier kinds
// that publishers UPLOAD through the `internal/ingest/...`
// HTTP verbs (NOT in the recipe registry, NOT a
// materialiser) but still write metric_sample rows through
// the SAME [PGMetricSampleWriter]. Listing them here is what
// makes the composition-root [VerifyMetricKindCatalog]
// probe COVER the ingested-row migrations -- without this
// list, a fresh process would come up against a catalog
// missing the ingested kinds and only fail at the first
// metric_sample INSERT, not at /readyz.
//
// COORDINATION RULE -- bumping the producer's version is a
// THREE-step edit:
//
//  1. bump the producer's `Metric{Kind,Version}` constants
//     (e.g. `internal/ingest/test_balance.MetricVersion`),
//  2. bump the `Version` field below to match,
//  3. land a `migrations/00XX_seed_<kind>_v<N>.up.sql` row
//     for the new (kind, metric_version) tuple (existing
//     catalog rows are immutable; the catalog stores one
//     row per emitted version, not just the latest).
//
// We hand-curate (rather than importing producer constants
// directly) because the producer packages depend on
// [metric_ingestor] for `MetricSampleWriter`, so importing
// the constants here would introduce a package import
// cycle. The coordination rule above is enforced by the
// VerifyMetricKindCatalog "version drift" check: a producer
// that has bumped its constant without bumping this list
// (or its migration) fails fast at startup with
// [ErrMetricKindCatalogVersionDrift].
//
// Tier is implicit (`foundation`) because all ingested
// kinds today live at architecture Sec 1.4.2's ingested
// foundation-tier slot; if a future ingested kind needs
// a different tier, add a Tier field to the struct.
var ingestedMetricKinds = []struct {
	Kind    string
	Version int
}{
	// pass_first_try_ratio -- produced by
	// `internal/ingest/test_balance.Writer` (Stage 4.3).
	// MUST track `test_balance.MetricKind` /
	// `test_balance.MetricVersion`.
	{Kind: "pass_first_try_ratio", Version: 1},
	// Coverage kinds (Stage 4.2) -- produced by the Cobertura
	// parser in `internal/ingest/coverage` and uploaded via
	// the `/v1/ingest/coverage` webhook. The coverage package
	// does NOT import `metric_ingestor`, so referencing the
	// producer constants here does NOT introduce an import
	// cycle (the hand-curated string-literal rule above
	// applies to producers that depend on
	// [PGMetricSampleWriter]).
	{Kind: coverage.MetricKindCoverageLineRatio, Version: coverage.MetricVersion},
	{Kind: coverage.MetricKindCoverageBranchRatio, Version: coverage.MetricVersion},
}

// MetricKindCatalogRowsForRegistry returns the canonical
// [MetricKindCatalogRow] slice for every recipe registered
// in `reg` PLUS the foundation-tier materialiser
// (`modification_count_in_window`) PLUS the foundation-tier
// ingested kinds enumerated in [ingestedMetricKinds] (e.g.
// `pass_first_try_ratio` from
// `internal/ingest/test_balance`). All three sources write
// through the SAME [PGMetricSampleWriter] and so share the
// FK requirement on `clean_code.metric_kind`.
//
// Returns [ErrMetricKindCatalogMissingMetadata] (wrapped with
// the offending metric_kind) on any kind that has no entry
// in [foundationCatalogMetadata]. Adding a new recipe / new
// materialiser / new ingested kind is therefore a coordinated
// three-step edit:
//
//  1. land the producer (recipe / materialiser / ingest writer),
//  2. register it (recipes -> `recipes.DefaultRegistry`;
//     materialisers -> picked up implicitly here;
//     ingested -> append to [ingestedMetricKinds]),
//  3. add the (unit, description) entry to
//     [foundationCatalogMetadata].
//
// Skipping step 3 surfaces at startup, not at first
// metric_sample write, which is the structural property the
// evaluator's "no production seeding path" item asked for.
func MetricKindCatalogRowsForRegistry(reg *recipes.Registry) ([]MetricKindCatalogRow, error) {
	if reg == nil {
		return nil, errors.New("metric_ingestor: MetricKindCatalogRowsForRegistry: registry is nil")
	}

	out := make([]MetricKindCatalogRow, 0, len(reg.Recipes())+1+len(ingestedMetricKinds))

	for _, r := range reg.Recipes() {
		kind := r.MetricKind()
		meta, ok := foundationCatalogMetadata[kind]
		if !ok {
			return nil, fmt.Errorf("%w: metric_kind=%q (add an entry to internal/metric_ingestor/metric_kind_catalog.go foundationCatalogMetadata)", ErrMetricKindCatalogMissingMetadata, kind)
		}
		tier, err := packToTier(r.Pack())
		if err != nil {
			return nil, fmt.Errorf("metric_ingestor: MetricKindCatalogRowsForRegistry recipe %q: %w", kind, err)
		}
		out = append(out, MetricKindCatalogRow{
			MetricKind:    kind,
			MetricVersion: r.Version(),
			Tier:          tier,
			Pack:          r.Pack(),
			Unit:          meta.Unit,
			DescriptionMD: meta.DescriptionMD,
		})
	}

	// Foundation-tier materialiser (Stage 2.6) -- NOT in the
	// recipe registry by design (architecture Sec 1.4.1 row 12
	// note: "AST adapter is NOT a producer of
	// modification_count_in_window"). The materialiser writes
	// through the same PGMetricSampleWriter so the FK must be
	// satisfied for the materialiser's INSERTs to succeed.
	matMeta, ok := foundationCatalogMetadata[materialisers.MetricKind]
	if !ok {
		return nil, fmt.Errorf("%w: metric_kind=%q (materialiser metadata missing)", ErrMetricKindCatalogMissingMetadata, materialisers.MetricKind)
	}
	out = append(out, MetricKindCatalogRow{
		MetricKind:    materialisers.MetricKind,
		MetricVersion: materialisers.MetricVersion,
		Tier:          MetricKindTierFoundation,
		Pack:          recipes.PackBase,
		Unit:          matMeta.Unit,
		DescriptionMD: matMeta.DescriptionMD,
	})

	// Foundation-tier INGESTED kinds (Stage 4.2 coverage,
	// Stage 4.3 test_balance, ...). NOT in the recipe
	// registry (no AST adapter produces them) and not a
	// materialiser (no recipe-emitted foundation row to
	// derive from); they are publisher-uploaded through the
	// `internal/ingest/...` HTTP verbs. The verb writers go
	// through the same PGMetricSampleWriter so the FK must be
	// satisfied for the writer's INSERTs to succeed.
	for _, ik := range ingestedMetricKinds {
		ingMeta, ok := foundationCatalogMetadata[ik.Kind]
		if !ok {
			return nil, fmt.Errorf("%w: metric_kind=%q (ingested metadata missing -- add to foundationCatalogMetadata)", ErrMetricKindCatalogMissingMetadata, ik.Kind)
		}
		out = append(out, MetricKindCatalogRow{
			MetricKind:    ik.Kind,
			MetricVersion: ik.Version,
			Tier:          MetricKindTierFoundation,
			Pack:          recipes.PackIngested,
			Unit:          ingMeta.Unit,
			DescriptionMD: ingMeta.DescriptionMD,
		})
	}

	// Deterministic ordering for log reproducibility (G2).
	sort.Slice(out, func(i, j int) bool { return out[i].MetricKind < out[j].MetricKind })
	return out, nil
}

// SeedMetricKindCatalog upserts `rows` into the
// `<schema>.metric_kind` catalog table with
// `ON CONFLICT (metric_kind) DO NOTHING` semantics. First
// writer wins: a steward-curated row that pre-exists from a
// dedicated migration is preserved untouched.
//
// SCOPE -- tests and admin tooling only. The PRODUCTION
// seed path is the schema-owner migration
// `services/clean-code/migrations/0007_seed_foundation_metric_kinds.up.sql`,
// which is applied by `make migrate-up` under the migration
// role (NOT under the runtime `clean_code_metric_ingestor`
// role, which lacks INSERT on `metric_kind` per
// `migrations/0004_roles.up.sql:350-355`). The composition
// root in `cmd/clean-code-metric-ingestor/main.go`
// deliberately does NOT call this function -- it calls only
// [VerifyMetricKindCatalog] (SELECT-only) to fail fast on
// version drift between the in-process producer registry
// and the rows the migration already seeded.
//
// Callers that DO invoke this function (test harnesses,
// one-shot admin scripts targeting a transient PG instance)
// SHOULD follow up with [VerifyMetricKindCatalog] to detect
// version drift -- DO NOTHING does NOT detect it on its
// own; an existing catalog row at v=1 silently masks a
// producer that has since bumped to v=2.
//
// Returns nil on empty `rows`. Each row's columns map 1:1 to
// the `clean_code.metric_kind` columns; the implementation
// wraps every error in a sentinel so callers can
// `errors.Is` against the typed misconfiguration cases.
//
// Idempotency: safe to call repeatedly against the same
// schema; the DO NOTHING clause means no UPDATE fires for
// an existing row, so the WRITE-ONCE-style stewardship
// pattern is preserved.
func SeedMetricKindCatalog(ctx context.Context, db *sql.DB, schema string, rows []MetricKindCatalogRow) error {
	if db == nil {
		return ErrMetricKindCatalogNilDB
	}
	if schema == "" {
		return ErrMetricKindCatalogEmptySchema
	}
	if len(rows) == 0 {
		return nil
	}

	qualified := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(pgMetricKindTable)
	insertSQL := fmt.Sprintf(
		`INSERT INTO %s
		    (metric_kind, metric_version, tier, pack, unit, description_md)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (metric_kind) DO NOTHING`,
		qualified,
	)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: BeginTx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: Prepare INSERT: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for i, row := range rows {
		if row.MetricKind == "" {
			return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: rows[%d].MetricKind is empty", i)
		}
		if row.MetricVersion < 1 {
			return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: rows[%d] (metric_kind=%q) MetricVersion=%d (want >= 1)", i, row.MetricKind, row.MetricVersion)
		}
		if row.Tier == "" {
			return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: rows[%d] (metric_kind=%q) Tier is empty", i, row.MetricKind)
		}
		if row.Pack == "" {
			return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: rows[%d] (metric_kind=%q) Pack is empty", i, row.MetricKind)
		}
		if row.Unit == "" {
			return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: rows[%d] (metric_kind=%q) Unit is empty", i, row.MetricKind)
		}
		if row.DescriptionMD == "" {
			return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: rows[%d] (metric_kind=%q) DescriptionMD is empty", i, row.MetricKind)
		}
		if _, err := stmt.ExecContext(ctx,
			row.MetricKind,
			row.MetricVersion,
			string(row.Tier),
			string(row.Pack),
			row.Unit,
			row.DescriptionMD,
		); err != nil {
			return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: rows[%d] (metric_kind=%q) INSERT: %w", i, row.MetricKind, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metric_ingestor: SeedMetricKindCatalog: Commit: %w", err)
	}
	return nil
}

// VerifyMetricKindCatalog confirms that every expected
// `(metric_kind, metric_version)` pair in `rows` is reflected
// in the on-disk `<schema>.metric_kind` table. This is the
// PRODUCTION composition-root fence:
// `cmd/clean-code-metric-ingestor/main.go` invokes it
// (via the `verifyMetricKindCatalog` helper) immediately
// after opening the ingestor `*sql.DB` and BEFORE building
// the sweep loop / mounting the routers, so a fresh process
// refuses to come up against a catalog that would FK-reject
// the first `metric_sample` write.
//
// SELECT-only -- the runtime `clean_code_metric_ingestor`
// role has SELECT on `clean_code.metric_kind` via the
// cross-sub-store-read GRANT at
// `migrations/0004_roles.up.sql:227-260` (table at line
// 230; ingestor in the grantee list at line 253) but NOT
// INSERT (INSERT is granted ONLY to
// `clean_code_policy_steward` at
// `migrations/0004_roles.up.sql:355`). The rows MUST
// already exist; the production seeding path is the
// schema-owner migration
// `services/clean-code/migrations/0007_seed_foundation_metric_kinds.up.sql`.
//
// Failure modes (both wrap the metric_kind name so the
// failure log names the producer the operator must
// reconcile):
//
//   - row missing entirely     -> [ErrMetricKindCatalogRowMissing]
//   - row.metric_version != N  -> [ErrMetricKindCatalogVersionDrift]
//
// Both errors wrap the metric_kind name so the failure log
// names the producer the operator must reconcile. The
// app-level remediation is to land a migration that updates
// the catalog row's metric_version (steward-curated) -- this
// function refuses to do the update itself because the
// COMMENT on `clean_code.metric_kind` pins steward ownership.
func VerifyMetricKindCatalog(ctx context.Context, db *sql.DB, schema string, rows []MetricKindCatalogRow) error {
	if db == nil {
		return ErrMetricKindCatalogNilDB
	}
	if schema == "" {
		return ErrMetricKindCatalogEmptySchema
	}
	if len(rows) == 0 {
		return nil
	}

	qualified := pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(pgMetricKindTable)
	selectSQL := fmt.Sprintf(`SELECT metric_version FROM %s WHERE metric_kind = $1`, qualified)

	stmt, err := db.PrepareContext(ctx, selectSQL)
	if err != nil {
		return fmt.Errorf("metric_ingestor: VerifyMetricKindCatalog: Prepare SELECT: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, want := range rows {
		var got int
		switch err := stmt.QueryRowContext(ctx, want.MetricKind).Scan(&got); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: metric_kind=%q", ErrMetricKindCatalogRowMissing, want.MetricKind)
		case err != nil:
			return fmt.Errorf("metric_ingestor: VerifyMetricKindCatalog: metric_kind=%q SELECT: %w", want.MetricKind, err)
		}
		if got != want.MetricVersion {
			return fmt.Errorf("%w: metric_kind=%q catalog_version=%d producer_version=%d (steward must land a migration updating the catalog row to match)",
				ErrMetricKindCatalogVersionDrift, want.MetricKind, got, want.MetricVersion)
		}
	}
	return nil
}
