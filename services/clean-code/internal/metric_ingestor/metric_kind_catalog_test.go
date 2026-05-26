package metric_ingestor_test

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

const pgMetricKindTestSchema = "clean_code_catalog_test"

func TestSeedMetricKindCatalog_RejectsNilDB(t *testing.T) {
	t.Parallel()
	err := metric_ingestor.SeedMetricKindCatalog(context.Background(), nil, "clean_code", nil)
	if !errors.Is(err, metric_ingestor.ErrMetricKindCatalogNilDB) {
		t.Errorf("SeedMetricKindCatalog(nil db): err=%v, want errors.Is ErrMetricKindCatalogNilDB", err)
	}
}

func TestSeedMetricKindCatalog_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, _ := sqlmock.New()
	defer db.Close()
	err := metric_ingestor.SeedMetricKindCatalog(context.Background(), db, "", nil)
	if !errors.Is(err, metric_ingestor.ErrMetricKindCatalogEmptySchema) {
		t.Errorf("SeedMetricKindCatalog(empty schema): err=%v, want errors.Is ErrMetricKindCatalogEmptySchema", err)
	}
}

func TestSeedMetricKindCatalog_EmptyRowsIsNoop(t *testing.T) {
	t.Parallel()
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	// No DB expectations: empty rows must not open a transaction.
	if err := metric_ingestor.SeedMetricKindCatalog(context.Background(), db, "clean_code", nil); err != nil {
		t.Errorf("SeedMetricKindCatalog(nil rows): err=%v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSeedMetricKindCatalog_HappyPath pins the canonical SQL
// trace: BEGIN, PREPARE INSERT with ON CONFLICT DO NOTHING,
// EXEC per row, COMMIT.
func TestSeedMetricKindCatalog_HappyPath(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := []metric_ingestor.MetricKindCatalogRow{
		{
			MetricKind:    "cyclo",
			MetricVersion: 1,
			Tier:          metric_ingestor.MetricKindTierFoundation,
			Pack:          recipes.PackBase,
			Unit:          "count",
			DescriptionMD: "McCabe cyclomatic complexity.",
		},
		{
			MetricKind:    "loc",
			MetricVersion: 1,
			Tier:          metric_ingestor.MetricKindTierFoundation,
			Pack:          recipes.PackBase,
			Unit:          "count",
			DescriptionMD: "Source lines of code.",
		},
	}

	mock.ExpectBegin()
	prep := mock.ExpectPrepare(`INSERT\s+INTO\s+"` + pgMetricKindTestSchema + `"\."metric_kind".*ON\s+CONFLICT\s+\(metric_kind\)\s+DO\s+NOTHING`)
	for _, r := range rows {
		prep.ExpectExec().WithArgs(
			r.MetricKind, r.MetricVersion, string(r.Tier), string(r.Pack), r.Unit, r.DescriptionMD,
		).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	if err := metric_ingestor.SeedMetricKindCatalog(context.Background(), db, pgMetricKindTestSchema, rows); err != nil {
		t.Fatalf("SeedMetricKindCatalog: err=%v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSeedMetricKindCatalog_RollsBackOnExecError pins atomic
// batch semantics: any per-row failure rolls back the whole
// transaction (no partial-write surface).
func TestSeedMetricKindCatalog_RollsBackOnExecError(t *testing.T) {
	t.Parallel()
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()

	rows := []metric_ingestor.MetricKindCatalogRow{
		{MetricKind: "cyclo", MetricVersion: 1, Tier: metric_ingestor.MetricKindTierFoundation, Pack: recipes.PackBase, Unit: "count", DescriptionMD: "x"},
		{MetricKind: "loc", MetricVersion: 1, Tier: metric_ingestor.MetricKindTierFoundation, Pack: recipes.PackBase, Unit: "count", DescriptionMD: "y"},
	}

	wantErr := errors.New("simulated catalog INSERT failure")
	mock.ExpectBegin()
	prep := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO`))
	prep.ExpectExec().WillReturnResult(sqlmock.NewResult(0, 1))
	prep.ExpectExec().WillReturnError(wantErr)
	mock.ExpectRollback()

	err := metric_ingestor.SeedMetricKindCatalog(context.Background(), db, "clean_code", rows)
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("SeedMetricKindCatalog: err=%v, want wrapping of %v", err, wantErr)
	}
}

func TestSeedMetricKindCatalog_RejectsEmptyFields(t *testing.T) {
	t.Parallel()
	db, _, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()

	cases := []struct {
		name string
		row  metric_ingestor.MetricKindCatalogRow
	}{
		{"empty metric_kind", metric_ingestor.MetricKindCatalogRow{MetricVersion: 1, Tier: metric_ingestor.MetricKindTierFoundation, Pack: recipes.PackBase, Unit: "count", DescriptionMD: "x"}},
		{"zero version", metric_ingestor.MetricKindCatalogRow{MetricKind: "cyclo", MetricVersion: 0, Tier: metric_ingestor.MetricKindTierFoundation, Pack: recipes.PackBase, Unit: "count", DescriptionMD: "x"}},
		{"empty tier", metric_ingestor.MetricKindCatalogRow{MetricKind: "cyclo", MetricVersion: 1, Pack: recipes.PackBase, Unit: "count", DescriptionMD: "x"}},
		{"empty pack", metric_ingestor.MetricKindCatalogRow{MetricKind: "cyclo", MetricVersion: 1, Tier: metric_ingestor.MetricKindTierFoundation, Unit: "count", DescriptionMD: "x"}},
		{"empty unit", metric_ingestor.MetricKindCatalogRow{MetricKind: "cyclo", MetricVersion: 1, Tier: metric_ingestor.MetricKindTierFoundation, Pack: recipes.PackBase, DescriptionMD: "x"}},
		{"empty description", metric_ingestor.MetricKindCatalogRow{MetricKind: "cyclo", MetricVersion: 1, Tier: metric_ingestor.MetricKindTierFoundation, Pack: recipes.PackBase, Unit: "count"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := metric_ingestor.SeedMetricKindCatalog(context.Background(), db, "clean_code", []metric_ingestor.MetricKindCatalogRow{tc.row})
			if err == nil {
				t.Fatalf("SeedMetricKindCatalog with %s: err=nil, want validation error", tc.name)
			}
		})
	}
}

// TestMetricKindCatalogRowsForRegistry_CoversDefaultRegistry
// pins that the canonical foundation-tier producers
// (registered recipes PLUS the modification_count materialiser)
// resolve to the metadata table; if a kind is added without a
// corresponding metadata entry, this test fails at compile
// (build gate) and at run time.
func TestMetricKindCatalogRowsForRegistry_CoversDefaultRegistry(t *testing.T) {
	t.Parallel()
	reg := recipes.DefaultRegistry()
	rows, err := metric_ingestor.MetricKindCatalogRowsForRegistry(reg)
	if err != nil {
		t.Fatalf("MetricKindCatalogRowsForRegistry: err=%v, want nil", err)
	}
	// 6 registered recipes (cyclo, cognitive_complexity, loc,
	// lcom4, fan_in, fan_out) + 1 materialiser
	// (modification_count_in_window) + 2 external-ingest
	// rows (coverage_line_ratio, coverage_branch_ratio) = 9
	// catalog rows.
	const want = 9
	if got := len(rows); got != want {
		t.Errorf("rows=%d, want %d (foundation registry + materialiser + external-ingest coverage); got=%v", got, want, kindsOf(rows))
	}
	expected := map[string]bool{
		"cyclo": false, "cognitive_complexity": false, "loc": false,
		"lcom4": false, "fan_in": false, "fan_out": false,
		"modification_count_in_window": false,
		"coverage_line_ratio":          false,
		"coverage_branch_ratio":        false,
	}
	for _, r := range rows {
		if _, ok := expected[r.MetricKind]; !ok {
			t.Errorf("unexpected metric_kind in catalog rows: %q", r.MetricKind)
			continue
		}
		expected[r.MetricKind] = true
		if r.MetricVersion < 1 {
			t.Errorf("rows[%q].MetricVersion=%d (want >= 1)", r.MetricKind, r.MetricVersion)
		}
		if r.Tier != metric_ingestor.MetricKindTierFoundation {
			t.Errorf("rows[%q].Tier=%q (want foundation -- the registry + materialiser are all foundation-tier)", r.MetricKind, r.Tier)
		}
		if r.Unit == "" || r.DescriptionMD == "" {
			t.Errorf("rows[%q] empty Unit (%q) or DescriptionMD (%q)", r.MetricKind, r.Unit, r.DescriptionMD)
		}
	}
	for k, seen := range expected {
		if !seen {
			t.Errorf("expected metric_kind=%q not produced by MetricKindCatalogRowsForRegistry", k)
		}
	}
}

func TestMetricKindCatalogRowsForRegistry_RejectsNilRegistry(t *testing.T) {
	t.Parallel()
	if _, err := metric_ingestor.MetricKindCatalogRowsForRegistry(nil); err == nil {
		t.Errorf("MetricKindCatalogRowsForRegistry(nil): err=nil, want error")
	}
}

func TestVerifyMetricKindCatalog_RejectsNilDB(t *testing.T) {
	t.Parallel()
	err := metric_ingestor.VerifyMetricKindCatalog(context.Background(), nil, "clean_code", []metric_ingestor.MetricKindCatalogRow{{MetricKind: "cyclo", MetricVersion: 1}})
	if !errors.Is(err, metric_ingestor.ErrMetricKindCatalogNilDB) {
		t.Errorf("VerifyMetricKindCatalog(nil db): err=%v, want errors.Is ErrMetricKindCatalogNilDB", err)
	}
}

func TestVerifyMetricKindCatalog_DetectsMissingRow(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectPrepare(`SELECT\s+metric_version\s+FROM\s+"clean_code"\."metric_kind"`).
		ExpectQuery().WithArgs("cyclo").WillReturnError(sql.ErrNoRows)

	want := []metric_ingestor.MetricKindCatalogRow{{MetricKind: "cyclo", MetricVersion: 1}}
	err = metric_ingestor.VerifyMetricKindCatalog(context.Background(), db, "clean_code", want)
	if err == nil || !errors.Is(err, metric_ingestor.ErrMetricKindCatalogRowMissing) {
		t.Errorf("VerifyMetricKindCatalog: err=%v, want errors.Is ErrMetricKindCatalogRowMissing", err)
	}
}

func TestVerifyMetricKindCatalog_DetectsVersionDrift(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectPrepare(`SELECT\s+metric_version\s+FROM\s+"clean_code"\."metric_kind"`).
		ExpectQuery().WithArgs("cyclo").
		WillReturnRows(sqlmock.NewRows([]string{"metric_version"}).AddRow(1))

	want := []metric_ingestor.MetricKindCatalogRow{{MetricKind: "cyclo", MetricVersion: 2}}
	err = metric_ingestor.VerifyMetricKindCatalog(context.Background(), db, "clean_code", want)
	if err == nil || !errors.Is(err, metric_ingestor.ErrMetricKindCatalogVersionDrift) {
		t.Errorf("VerifyMetricKindCatalog: err=%v, want errors.Is ErrMetricKindCatalogVersionDrift", err)
	}
}

func TestVerifyMetricKindCatalog_HappyPath(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	prep := mock.ExpectPrepare(`SELECT\s+metric_version\s+FROM\s+"clean_code"\."metric_kind"`)
	prep.ExpectQuery().WithArgs("cyclo").
		WillReturnRows(sqlmock.NewRows([]string{"metric_version"}).AddRow(1))
	prep.ExpectQuery().WithArgs("loc").
		WillReturnRows(sqlmock.NewRows([]string{"metric_version"}).AddRow(1))

	want := []metric_ingestor.MetricKindCatalogRow{
		{MetricKind: "cyclo", MetricVersion: 1},
		{MetricKind: "loc", MetricVersion: 1},
	}
	if err := metric_ingestor.VerifyMetricKindCatalog(context.Background(), db, "clean_code", want); err != nil {
		t.Errorf("VerifyMetricKindCatalog: err=%v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func kindsOf(rows []metric_ingestor.MetricKindCatalogRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.MetricKind
	}
	return out
}
