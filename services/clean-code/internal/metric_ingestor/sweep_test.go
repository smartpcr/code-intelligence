package metric_ingestor_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/materialisers"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// fixedRepoID + fixedScanRunID + scope_ids are pinned uuid
// literals so tests are byte-deterministic across CI runs.
var (
	fixedRepoID    = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))
	fixedScanRunID = uuid.Must(uuid.FromString("aaaaaaaa-1111-2222-3333-444444444444"))
	fooScopeID     = uuid.Must(uuid.FromString("bbbbbbbb-0000-0000-0000-000000000001"))
	barScopeID     = uuid.Must(uuid.FromString("bbbbbbbb-0000-0000-0000-000000000002"))
)

// fixedNow is the deterministic "wall-clock" the materialiser
// captures. Picked so every fixture date is in-window
// (`now - 90d <= d <= now`).
func fixedNow() time.Time {
	return time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
}

// newSweep wires a ChurnSweep with deterministic dependencies:
// a frozen clock, a [MapScopeResolver] for the supplied files,
// and an [InMemoryMetricSampleWriter]. Returns the sweep + the
// writer so the test can both drive Run and assert on
// `writer.Records()`.
func newSweep(
	t *testing.T,
	windowDays int,
	files map[string]uuid.UUID,
) (*metric_ingestor.ChurnSweep, *metric_ingestor.InMemoryMetricSampleWriter, *churn.MapScopeResolver) {
	t.Helper()
	resolver := churn.NewMapScopeResolver()
	for path, sid := range files {
		resolver.Add(fixedRepoID, path, sid, recipes.ScopeRef{
			LocalID:       sid.String(),
			Kind:          scope.KindFile,
			QualifiedName: path,
			Path:          path,
		})
	}
	mat := materialisers.NewMaterialiserWithClock(windowDays, fixedNow)
	hyd := churn.NewHydrator(resolver)
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	return sweep, writer, resolver
}

// goodScanRun returns a [ScanRunContext] that passes every
// pre-sweep guard.
func goodScanRun() metric_ingestor.ScanRunContext {
	return metric_ingestor.ScanRunContext{
		ID:     fixedScanRunID,
		Kind:   metric_ingestor.ScanRunKindExternalPerRow,
		RepoID: fixedRepoID,
	}
}

// TestChurnSweep_HappyPath_EmitsOneRecordPerScope -- canonical
// success case: 3 churn rows (2 distinct files, 3 distinct
// SHAs) produces 2 [MetricSampleRecord]s with `value` = count
// of unique in-window SHAs per scope.
func TestChurnSweep_HappyPath_EmitsOneRecordPerScope(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
		"internal/bar.go": barScopeID,
	})
	payload := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-24 * time.Hour)},
			{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-48 * time.Hour)},
			{SHA: "cccccccccccccccccccccccccccccccccccccccc", FilePath: "internal/bar.go", ModifiedAt: fixedNow().Add(-72 * time.Hour)},
		},
	}
	got, err := sweep.Run(context.Background(), goodScanRun(), payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.RowsHydrated != 3 {
		t.Errorf("RowsHydrated = %d, want 3", got.RowsHydrated)
	}
	if got.SamplesWritten != 2 {
		t.Errorf("SamplesWritten = %d, want 2 (one per file scope)", got.SamplesWritten)
	}
	records := writer.Records()
	if len(records) != 2 {
		t.Fatalf("writer.Records() len = %d, want 2", len(records))
	}
	// Index by scope_id so the assertions are order-tolerant.
	byScope := make(map[uuid.UUID]metric_ingestor.MetricSampleRecord, len(records))
	for _, r := range records {
		byScope[r.ScopeID] = r
	}
	foo, ok := byScope[fooScopeID]
	if !ok {
		t.Fatalf("no record for foo.go scope_id=%v", fooScopeID)
	}
	if foo.Value != 2 {
		t.Errorf("foo value = %v, want 2 (sha-A + sha-B)", foo.Value)
	}
	bar, ok := byScope[barScopeID]
	if !ok {
		t.Fatalf("no record for bar.go scope_id=%v", barScopeID)
	}
	if bar.Value != 1 {
		t.Errorf("bar value = %v, want 1 (sha-C only)", bar.Value)
	}

	// Canonical field assertions across every emitted record.
	for _, r := range records {
		if r.MetricKind != materialisers.MetricKind {
			t.Errorf("record %v: MetricKind = %q, want %q", r.ScopeID, r.MetricKind, materialisers.MetricKind)
		}
		if r.MetricVersion != materialisers.MetricVersion {
			t.Errorf("record %v: MetricVersion = %d, want %d", r.ScopeID, r.MetricVersion, materialisers.MetricVersion)
		}
		if r.Pack != recipes.PackBase {
			t.Errorf("record %v: Pack = %q, want %q", r.ScopeID, r.Pack, recipes.PackBase)
		}
		if r.Source != recipes.SourceComputed {
			t.Errorf("record %v: Source = %q, want %q (tech-spec Sec 4.11)", r.ScopeID, r.Source, recipes.SourceComputed)
		}
		if r.Attrs[materialisers.AttrProvenance] != materialisers.AttrProvenanceValue {
			t.Errorf("record %v: attrs[provenance] = %q, want %q (C19)", r.ScopeID, r.Attrs[materialisers.AttrProvenance], materialisers.AttrProvenanceValue)
		}
		if r.Attrs[materialisers.AttrWindowDays] != "90" {
			t.Errorf("record %v: attrs[window_days] = %q, want %q (tech-spec Sec 8.2)", r.ScopeID, r.Attrs[materialisers.AttrWindowDays], "90")
		}
		if len(r.Attrs) != 2 {
			t.Errorf("record %v: len(attrs) = %d, want 2 (closed-set: provenance, window_days)", r.ScopeID, len(r.Attrs))
		}
		if r.ProducerRunID != fixedScanRunID {
			t.Errorf("record %v: ProducerRunID = %v, want %v", r.ScopeID, r.ProducerRunID, fixedScanRunID)
		}
		if r.RepoID != fixedRepoID {
			t.Errorf("record %v: RepoID = %v, want %v", r.ScopeID, r.RepoID, fixedRepoID)
		}
		if r.SampleID == uuid.Nil {
			t.Errorf("record %v: SampleID is the zero UUID", r.ScopeID)
		}
	}
}

// TestChurnSweep_StampsLatestInWindowSHA -- the
// `MetricSample.sha` column is the LATEST in-window commit
// SHA per scope (the materialiser collapses N commits into
// one row; the row's natural identity SHA is the most recent
// one).
func TestChurnSweep_StampsLatestInWindowSHA(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	payload := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "0000000000000000000000000000000000000000", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-72 * time.Hour)},
			{SHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-1 * time.Hour)},
			{SHA: "dddddddddddddddddddddddddddddddddddddddd", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-36 * time.Hour)},
		},
	}
	_, err := sweep.Run(context.Background(), goodScanRun(), payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	records := writer.Records()
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	if records[0].SHA != "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
		t.Errorf("SHA = %q, want %q (latest in-window committer date wins)", records[0].SHA, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	}
	if records[0].Value != 3 {
		t.Errorf("Value = %v, want 3 (three unique SHAs)", records[0].Value)
	}
}

// TestChurnSweep_StampsLatestInWindowSHA_ExcludesFutureDated --
// evaluator iter-2 #2: when a scope's most-recent ModifiedAt
// is a FUTURE-dated row (clock-skewed publisher), the
// materialiser drops it from the count, and the sweep MUST
// drop it from the latest-SHA selection too. The
// MetricSample.sha column should reflect the latest IN-WINDOW
// commit, NEVER a row the materialiser did not count.
//
// Before the iter-3 fix, the sweep built its `latestByKey`
// from ALL hydrated rows, so the future-dated SHA leaked onto
// the persisted MetricSample.sha while the count came from the
// older in-window rows. The
// [Materialiser.MaterialiseWithDetails] entrypoint now
// computes both from the same in-window row set under a
// single now() capture.
func TestChurnSweep_StampsLatestInWindowSHA_ExcludesFutureDated(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	payload := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			// In-window rows.
			{SHA: "1111111111111111111111111111111111111111", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-72 * time.Hour)},
			{SHA: "2222222222222222222222222222222222222222", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-1 * time.Hour)},
			// Future-dated row -- clock-skewed publisher.
			// `sha-future` would be the "latest" by ModifiedAt
			// across the FULL hydrated set, but the
			// materialiser drops it (`r.ModifiedAt.After(now)`).
			// The sweep MUST stamp `sha-in-2` -- the latest
			// IN-WINDOW commit -- not `sha-future`.
			{SHA: "ffffffffffffffffffffffffffffffffffffffff", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(48 * time.Hour)},
		},
	}
	_, err := sweep.Run(context.Background(), goodScanRun(), payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	records := writer.Records()
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	if records[0].SHA == "ffffffffffffffffffffffffffffffffffffffff" {
		t.Errorf("MetricSample.sha = %q -- the materialiser dropped sha-future as future-dated; the sweep MUST NOT stamp it (evaluator iter-2 #2)", records[0].SHA)
	}
	if records[0].SHA != "2222222222222222222222222222222222222222" {
		t.Errorf("MetricSample.sha = %q, want %q (latest in-window commit)", records[0].SHA, "2222222222222222222222222222222222222222")
	}
	if records[0].Value != 2 {
		t.Errorf("Value = %v, want 2 (only in-window SHAs counted -- future-dated dropped)", records[0].Value)
	}
}

// TestChurnSweep_StampsLatestInWindowSHA_ExcludesOutOfWindow --
// complementary to the future-dated test: when a scope's
// most-recent ModifiedAt is OLDER than the cutoff, the
// materialiser drops it; the sweep MUST drop it from the
// latest-SHA selection too. If the only remaining rows are
// in-window, the sweep emits a record stamped with the
// LATEST IN-WINDOW SHA (not the older out-of-window one).
func TestChurnSweep_StampsLatestInWindowSHA_ExcludesOutOfWindow(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	payload := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			// In-window rows.
			{SHA: "1111111111111111111111111111111111111111", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-72 * time.Hour)},
			{SHA: "2222222222222222222222222222222222222222", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-30 * time.Hour)},
			// Out-of-window row (200 days old, > 90-day window).
			{SHA: "0000000000000000000000000000000000000000", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-200 * 24 * time.Hour)},
		},
	}
	_, err := sweep.Run(context.Background(), goodScanRun(), payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	records := writer.Records()
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}
	if records[0].SHA != "2222222222222222222222222222222222222222" {
		t.Errorf("MetricSample.sha = %q, want %q (out-of-window sha-old MUST be excluded from latest-SHA selection)", records[0].SHA, "2222222222222222222222222222222222222222")
	}
	if records[0].Value != 2 {
		t.Errorf("Value = %v, want 2 (only in-window SHAs counted)", records[0].Value)
	}
}

// TestChurnSweep_OutOfWindow_NoRecords -- every payload row
// older than the cutoff yields zero records (no zero-fill;
// implementation-plan Stage 2.6 line 253) and writer is NOT
// called.
func TestChurnSweep_OutOfWindow_NoRecords(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	payload := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			// All 200 days old -- well outside the 90-day window.
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-200 * 24 * time.Hour)},
		},
	}
	got, err := sweep.Run(context.Background(), goodScanRun(), payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.SamplesWritten != 0 {
		t.Errorf("SamplesWritten = %d, want 0 (no zero-fill)", got.SamplesWritten)
	}
	if got.RowsHydrated != 1 {
		t.Errorf("RowsHydrated = %d, want 1 (the row was hydrated, then materialiser dropped it)", got.RowsHydrated)
	}
	if len(writer.Records()) != 0 {
		t.Errorf("writer.Records() len = %d, want 0", len(writer.Records()))
	}
}

// TestChurnSweep_RejectsInvalidScanRunKind -- the canon-guard
// rejects any kind NOT in [AllowedScanRunKinds] BEFORE
// touching the hydrator or writer.
//
// [ScanRunKindFull] and [ScanRunKindDelta] are intentionally
// NOT in this rejection set -- the Stage 2.6 requirement
// "materialiser runs inside the same ScanRun as the foundation
// recipes" makes them VALID parent kinds; see
// [TestChurnSweep_AcceptsFoundationScanRunKinds].
func TestChurnSweep_RejectsInvalidScanRunKind(t *testing.T) {
	t.Parallel()
	cases := []string{
		metric_ingestor.ScanRunKindExternalSingle,
		metric_ingestor.ScanRunKindRetract,
		"", // missing
		"bogus",
	}
	for _, kind := range cases {
		kind := kind
		t.Run(fmt.Sprintf("kind=%q", kind), func(t *testing.T) {
			t.Parallel()
			sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
				"internal/foo.go": fooScopeID,
			})
			scanRun := goodScanRun()
			scanRun.Kind = kind
			payload := &churn.Payload{
				RepoID: fixedRepoID,
				Rows: []churn.PayloadRow{
					{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-time.Hour)},
				},
			}
			_, err := sweep.Run(context.Background(), scanRun, payload)
			if !errors.Is(err, metric_ingestor.ErrInvalidScanRunKind) {
				t.Fatalf("err = %v, want errors.Is ErrInvalidScanRunKind", err)
			}
			if len(writer.Records()) != 0 {
				t.Errorf("writer was called despite ScanRun kind guard; records = %d", len(writer.Records()))
			}
		})
	}
}

// TestChurnSweep_AcceptsFoundationScanRunKinds -- the Stage
// 2.6 detailed requirement pins "Materialiser runs as part of
// the Metric Ingestor (same writer-ownership role) inside the
// same ScanRun as the foundation recipes". The foundation
// recipes run under `kind='full'` (initial scan) or
// `kind='delta'` (incremental scan -- impl-plan Stage 3.2 line
// 290); the sweep MUST accept both. Also accepts the
// churn-only `external_per_row` for the standalone webhook
// path.
func TestChurnSweep_AcceptsFoundationScanRunKinds(t *testing.T) {
	t.Parallel()
	cases := []string{
		metric_ingestor.ScanRunKindFull,
		metric_ingestor.ScanRunKindDelta,
		metric_ingestor.ScanRunKindExternalPerRow,
	}
	for _, kind := range cases {
		kind := kind
		t.Run(fmt.Sprintf("kind=%q", kind), func(t *testing.T) {
			t.Parallel()
			sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
				"internal/foo.go": fooScopeID,
			})
			scanRun := goodScanRun()
			scanRun.Kind = kind
			payload := &churn.Payload{
				RepoID: fixedRepoID,
				Rows: []churn.PayloadRow{
					{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-time.Hour)},
				},
			}
			got, err := sweep.Run(context.Background(), scanRun, payload)
			if err != nil {
				t.Fatalf("Run(kind=%q): %v", kind, err)
			}
			if got.SamplesWritten != 1 {
				t.Errorf("kind=%q: SamplesWritten = %d, want 1", kind, got.SamplesWritten)
			}
			if records := writer.Records(); len(records) != 1 {
				t.Fatalf("kind=%q: writer.Records() len = %d, want 1", kind, len(records))
			} else if records[0].ProducerRunID != fixedScanRunID {
				t.Errorf("kind=%q: ProducerRunID = %v, want %v", kind, records[0].ProducerRunID, fixedScanRunID)
			}
		})
	}
}

// TestAllowedScanRunKinds_Canon -- the closed set the sweep
// accepts. Pinned here so a future widening is a visible diff
// in this test.
func TestAllowedScanRunKinds_Canon(t *testing.T) {
	t.Parallel()
	got := metric_ingestor.AllowedScanRunKinds()
	want := []string{"full", "delta", "external_per_row"}
	if len(got) != len(want) {
		t.Fatalf("AllowedScanRunKinds() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllowedScanRunKinds()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Fresh-copy invariant: caller mutation does NOT leak.
	got[0] = "mutated"
	got2 := metric_ingestor.AllowedScanRunKinds()
	if got2[0] != "full" {
		t.Errorf("AllowedScanRunKinds()[0] after caller mutation = %q, want %q (must return fresh copy)", got2[0], "full")
	}
}

// TestChurnSweep_RejectsZeroScanRunID -- a zero scan_run_id at
// this layer is always an uninitialised caller value.
func TestChurnSweep_RejectsZeroScanRunID(t *testing.T) {
	t.Parallel()
	sweep, _, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	scanRun := goodScanRun()
	scanRun.ID = uuid.Nil
	_, err := sweep.Run(context.Background(), scanRun, &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow()},
		},
	})
	if !errors.Is(err, metric_ingestor.ErrZeroScanRunID) {
		t.Fatalf("err = %v, want errors.Is ErrZeroScanRunID", err)
	}
}

// TestChurnSweep_RejectsZeroRepoID -- a zero scan_run.repo_id
// at this layer is always an uninitialised caller value
// (evaluator iter-2 #3). The pre-sweep validator rejects it
// BEFORE the payload cross-check so the writer-ownership
// per-repo invariant is non-negotiable.
func TestChurnSweep_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	scanRun := goodScanRun()
	scanRun.RepoID = uuid.Nil
	_, err := sweep.Run(context.Background(), scanRun, &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow()},
		},
	})
	if !errors.Is(err, metric_ingestor.ErrZeroRepoID) {
		t.Fatalf("err = %v, want errors.Is ErrZeroRepoID", err)
	}
	if len(writer.Records()) != 0 {
		t.Errorf("writer was called despite zero repo_id; records = %d", len(writer.Records()))
	}
}

// TestScanRunContext_Validate_RejectsZeroRepoID -- the unit-level
// guard mirrors the integration guard above so a caller hitting
// Validate() directly gets the same canonical error.
func TestScanRunContext_Validate_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	c := goodScanRun()
	c.RepoID = uuid.Nil
	err := c.Validate()
	if !errors.Is(err, metric_ingestor.ErrZeroRepoID) {
		t.Errorf("Validate() = %v, want errors.Is ErrZeroRepoID", err)
	}
}

// TestChurnSweep_RejectsRepoIDMismatch -- the sweep refuses
// to mix repos in a single batch.
func TestChurnSweep_RejectsRepoIDMismatch(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	otherRepoID := uuid.Must(uuid.FromString("99999999-8888-7777-6666-555555555555"))
	scanRun := goodScanRun()
	scanRun.RepoID = otherRepoID
	_, err := sweep.Run(context.Background(), scanRun, &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow()},
		},
	})
	if !errors.Is(err, metric_ingestor.ErrRepoIDMismatch) {
		t.Fatalf("err = %v, want errors.Is ErrRepoIDMismatch", err)
	}
	if len(writer.Records()) != 0 {
		t.Errorf("writer was called despite repo_id mismatch; records = %d", len(writer.Records()))
	}
}

// TestChurnSweep_HydrateErrorPropagatesAndWriterNotCalled -- an
// unresolvable file aborts the entire sweep; writer-ownership
// atomicity demands no partial write.
func TestChurnSweep_HydrateErrorPropagatesAndWriterNotCalled(t *testing.T) {
	t.Parallel()
	// foo.go resolvable, bar.go NOT.
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	payload := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow()},
			{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FilePath: "internal/bar.go", ModifiedAt: fixedNow()},
		},
	}
	_, err := sweep.Run(context.Background(), goodScanRun(), payload)
	if !errors.Is(err, churn.ErrScopeResolutionFailed) {
		t.Fatalf("err = %v, want errors.Is ErrScopeResolutionFailed", err)
	}
	if len(writer.Records()) != 0 {
		t.Errorf("writer was called despite hydrate error; records = %d", len(writer.Records()))
	}
}

// TestChurnSweep_WriterErrorIsWrapped -- a failed
// [MetricSampleWriter.WriteBatch] surfaces as a wrapped
// [ErrWriterFailure] so callers can branch on errors.Is.
func TestChurnSweep_WriterErrorIsWrapped(t *testing.T) {
	t.Parallel()
	sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	wantInner := errors.New("simulated PG transient error")
	writer.FailNext(wantInner)
	payload := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-time.Hour)},
		},
	}
	_, err := sweep.Run(context.Background(), goodScanRun(), payload)
	if !errors.Is(err, metric_ingestor.ErrWriterFailure) {
		t.Fatalf("err = %v, want errors.Is ErrWriterFailure", err)
	}
	if !strings.Contains(err.Error(), "simulated PG transient error") {
		t.Errorf("wrapped err missing inner detail; got %v", err)
	}
}

// TestChurnSweep_RejectsNilPayload -- defensive guard for the
// HTTP handler's "decoded but pointer is still nil" edge.
func TestChurnSweep_RejectsNilPayload(t *testing.T) {
	t.Parallel()
	sweep, _, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	_, err := sweep.Run(context.Background(), goodScanRun(), nil)
	if err == nil {
		t.Fatalf("Run(nil): want error, got nil")
	}
	if !strings.Contains(err.Error(), "payload is nil") {
		t.Errorf("err message missing `payload is nil`; got %v", err)
	}
}

// TestChurnSweep_RecordOrderIsDeterministic -- two runs over
// the same input produce records in the same order (G2).
func TestChurnSweep_RecordOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	build := func() ([]metric_ingestor.MetricSampleRecord, error) {
		sweep, writer, _ := newSweep(t, materialisers.DefaultWindowDays, map[string]uuid.UUID{
			"internal/a.go": uuid.Must(uuid.FromString("11111111-aaaa-aaaa-aaaa-000000000001")),
			"internal/b.go": uuid.Must(uuid.FromString("11111111-aaaa-aaaa-aaaa-000000000002")),
			"internal/c.go": uuid.Must(uuid.FromString("11111111-aaaa-aaaa-aaaa-000000000003")),
		})
		payload := &churn.Payload{
			RepoID: fixedRepoID,
			Rows: []churn.PayloadRow{
				{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FilePath: "internal/a.go", ModifiedAt: fixedNow().Add(-time.Hour)},
				{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FilePath: "internal/b.go", ModifiedAt: fixedNow().Add(-2 * time.Hour)},
				{SHA: "cccccccccccccccccccccccccccccccccccccccc", FilePath: "internal/c.go", ModifiedAt: fixedNow().Add(-3 * time.Hour)},
			},
		}
		if _, err := sweep.Run(context.Background(), goodScanRun(), payload); err != nil {
			return nil, err
		}
		return writer.Records(), nil
	}
	r1, err := build()
	if err != nil {
		t.Fatalf("build 1: %v", err)
	}
	r2, err := build()
	if err != nil {
		t.Fatalf("build 2: %v", err)
	}
	if len(r1) != len(r2) {
		t.Fatalf("record counts differ: r1=%d r2=%d", len(r1), len(r2))
	}
	for i := range r1 {
		// SampleID is freshly minted per run -- exclude it
		// from the equality check; the rest of the row must
		// match.
		if r1[i].ScopeID != r2[i].ScopeID {
			t.Errorf("record %d: ScopeID differs between runs (r1=%v r2=%v)", i, r1[i].ScopeID, r2[i].ScopeID)
		}
		if r1[i].SHA != r2[i].SHA {
			t.Errorf("record %d: SHA differs (%q vs %q)", i, r1[i].SHA, r2[i].SHA)
		}
		if r1[i].Value != r2[i].Value {
			t.Errorf("record %d: Value differs (%v vs %v)", i, r1[i].Value, r2[i].Value)
		}
	}
}

// TestNewChurnSweep_PanicsOnNilMaterialiser -- composition-root
// wiring bug should fail loudly.
func TestNewChurnSweep_PanicsOnNilMaterialiser(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewChurnSweep(nil, ..., ...): want panic, got nil")
		}
	}()
	_ = metric_ingestor.NewChurnSweep(
		nil,
		churn.NewHydrator(churn.NewMapScopeResolver()),
		metric_ingestor.NewInMemoryMetricSampleWriter(),
	)
}

// TestNewChurnSweep_PanicsOnNilHydrator -- composition-root
// wiring bug should fail loudly.
func TestNewChurnSweep_PanicsOnNilHydrator(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewChurnSweep(..., nil, ...): want panic, got nil")
		}
	}()
	_ = metric_ingestor.NewChurnSweep(
		materialisers.NewMaterialiserWithClock(90, fixedNow),
		nil,
		metric_ingestor.NewInMemoryMetricSampleWriter(),
	)
}

// TestNewChurnSweep_PanicsOnNilWriter -- composition-root
// wiring bug should fail loudly.
func TestNewChurnSweep_PanicsOnNilWriter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewChurnSweep(..., ..., nil): want panic, got nil")
		}
	}()
	_ = metric_ingestor.NewChurnSweep(
		materialisers.NewMaterialiserWithClock(90, fixedNow),
		churn.NewHydrator(churn.NewMapScopeResolver()),
		nil,
	)
}

// TestInMemoryMetricSampleWriter_EmptyBatch_IsNoOp -- the
// writer contract: an empty batch is a no-op (no transaction,
// no error, no records appended).
func TestInMemoryMetricSampleWriter_EmptyBatch_IsNoOp(t *testing.T) {
	t.Parallel()
	w := metric_ingestor.NewInMemoryMetricSampleWriter()
	if err := w.WriteBatch(context.Background(), nil); err != nil {
		t.Errorf("WriteBatch(nil): err = %v, want nil", err)
	}
	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{}); err != nil {
		t.Errorf("WriteBatch([]): err = %v, want nil", err)
	}
	if got := w.Records(); len(got) != 0 {
		t.Errorf("writer received empty batches but Records() = %v", got)
	}
}

// TestInMemoryMetricSampleWriter_FailNextIsOneShot -- the
// failNext escape hatch fires exactly once.
func TestInMemoryMetricSampleWriter_FailNextIsOneShot(t *testing.T) {
	t.Parallel()
	w := metric_ingestor.NewInMemoryMetricSampleWriter()
	want := errors.New("boom")
	w.FailNext(want)
	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{{}}); err != want {
		t.Errorf("first WriteBatch: err = %v, want %v", err, want)
	}
	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{{SampleID: fixedScanRunID}}); err != nil {
		t.Errorf("second WriteBatch: err = %v, want nil (FailNext is one-shot)", err)
	}
	if got := w.Records(); len(got) != 1 {
		t.Errorf("after one armed failure + one success, Records() len = %d, want 1", len(got))
	}
}

// TestScanRunContext_Validate_HappyPath -- the validator
// accepts a fully-populated context.
func TestScanRunContext_Validate_HappyPath(t *testing.T) {
	t.Parallel()
	if err := goodScanRun().Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestScanRunKindExternalPerRow_MatchesChurnCanon -- the
// `metric_ingestor` re-export MUST string-equal the
// `churn`-side canon to avoid drift.
func TestScanRunKindExternalPerRow_MatchesChurnCanon(t *testing.T) {
	t.Parallel()
	if metric_ingestor.ScanRunKindExternalPerRow != churn.ScanRunKindExternalPerRow {
		t.Errorf("ScanRunKindExternalPerRow mismatch: metric_ingestor=%q churn=%q",
			metric_ingestor.ScanRunKindExternalPerRow, churn.ScanRunKindExternalPerRow)
	}
	if metric_ingestor.ScanRunKindExternalPerRow != "external_per_row" {
		t.Errorf("ScanRunKindExternalPerRow = %q, want %q (architecture Sec 4.4)",
			metric_ingestor.ScanRunKindExternalPerRow, "external_per_row")
	}
}
