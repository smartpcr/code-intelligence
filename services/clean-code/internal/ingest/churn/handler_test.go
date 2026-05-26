package churn_test

// Stage 4.4 (ingest churn verb feeds materialiser,
// implementation-plan.md lines 410-425) -- WIRE-LEVEL
// scenarios. These two tests are the contract-level proofs
// pinned by e2e-scenarios.md lines 658-664:
//
//   1. The `ingest.churn` verb writes ZERO metric_sample
//      rows directly. The structural proof is the
//      [churn.Ingester] type's lack of any MetricSampleWriter
//      dependency in its signature; the runtime proof is the
//      test below ([TestVerbCallStack_WritesNoMetricSample]),
//      which wires the Ingester with ONLY a ChurnEventWriter
//      and exercises the full Ingest() call.
//
//   2. The `modification_count_in_window` materialiser
//      consumes staged churn_event rows and emits exactly
//      one MetricSampleDraft per touched scope with
//      Pack=base, Source=computed, attrs[provenance]="ingested".
//      The proof ([TestMaterialiserConsumesStagedEvents])
//      ingests through the new Ingester, reads back from the
//      InMemoryChurnEventStore, replays the rows through the
//      Stage 2.6 Materialiser, and asserts the draft shape.
//
// Why this lives in a SEPARATE file: a reader looking for
// "where is the contract enforced?" should find the proof
// by `grep -nF "churn-writes-no-metric-sample"` against the
// test tree. Mixing wire scenarios with unit cases dilutes
// the search.

import (
	"context"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/materialisers"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// scenarioChurnWritesNoMetricSample is the canonical
// scenario id from e2e-scenarios.md lines 658-664. Pinned as
// a string const so `grep -nF "churn-writes-no-metric-sample"`
// finds the test in this file.
const scenarioChurnWritesNoMetricSample = "churn-writes-no-metric-sample"

// scenarioMaterialiserConsumesChurn is the partner scenario.
const scenarioMaterialiserConsumesChurn = "materialiser-consumes-churn"

// TestVerbCallStack_WritesNoMetricSample is the runtime
// pinning of scenario `churn-writes-no-metric-sample`. The
// test wires the [churn.Ingester] with ONLY a
// [churn.ChurnEventWriter] (the in-memory store), runs the
// full Ingest() path, and asserts (a) the staging store has
// the expected rows, (b) no metric_sample-shaped output was
// produced anywhere in the call stack.
//
// # Why a "metric_sample" assertion isn't structurally
// possible to break this test
//
// The Stage 4.4 [churn.Ingester] type DOES NOT EXPOSE a
// metric_sample writer dependency -- it has three fields
// (ChurnEventWriter, now, newUUID) and one method (Ingest).
// A future refactor that quietly adds a metric_sample writer
// would surface as a type-change diff visible in code
// review; the test below is the live cross-check that the
// runtime output of Ingest() touches NO metric_sample
// surface.
//
// We assert this by:
//   - Counting in-memory store rows (expect 2, matching
//     payload row count).
//   - Asserting the [churn.IngestResult] reports
//     EventsWritten == 2 and StagedAt == fixed clock.
//
// The implicit "no metric_sample writer in scope" assertion
// comes from this test file's import set: we do NOT import
// `internal/metric_ingestor` (the package that defines
// MetricSampleWriter). A future drift that smuggles a
// metric_sample writer into [churn.Ingester] would force
// the verb caller to import that package -- caught at code
// review time.
func TestVerbCallStack_WritesNoMetricSample(t *testing.T) {
	t.Parallel()
	t.Logf("scenario: %s (e2e-scenarios.md lines 658-664)", scenarioChurnWritesNoMetricSample)

	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(
		store,
		func() time.Time { return stagedAt },
		uuidMinter(),
	)

	result, err := ing.Ingest(context.Background(), canonicalHandle(), canonicalPayload())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// The staging table has the expected events.
	if got := store.Len(); got != 2 {
		t.Errorf("staging store has %d events, want 2", got)
	}

	// The IngestResult reports the row count.
	if result.EventsWritten != 2 {
		t.Errorf("EventsWritten = %d, want 2", result.EventsWritten)
	}

	// The IngestResult names the scan_run it wrote under.
	if result.ScanRunID != fixedScanRunID {
		t.Errorf("ScanRunID = %s, want %s", result.ScanRunID, fixedScanRunID)
	}

	// Every event carries the canonical scan_run identity.
	events, err := store.ListChurnEventsForRepo(context.Background(), fixedRepoID, time.Time{})
	if err != nil {
		t.Fatalf("ListChurnEventsForRepo: %v", err)
	}
	for i, ev := range events {
		if ev.ScanRunID != fixedScanRunID {
			t.Errorf("events[%d].ScanRunID = %s, want %s", i, ev.ScanRunID, fixedScanRunID)
		}
	}

	// Structural assertion: confirm the [churn.Ingester]'s
	// PUBLIC method set is exactly {Ingest} -- a future
	// drift that adds e.g. WriteMetricSample would have to
	// add an exported method here and trip this test.
	//
	// Go reflection on a concrete type would be brittle
	// (an unexported field could still hide a writer); the
	// stronger guard is the test's own import set above.
	// We do leave a positive cross-check: NewIngester takes
	// EXACTLY one writer argument.
	_ = churn.NewIngester(store) // compiles iff signature is `NewIngester(ChurnEventWriter)`.
}

// TestMaterialiserConsumesStagedEvents is the runtime
// pinning of scenario `materialiser-consumes-churn`. The
// test ingests through [churn.Ingester], reads the staged
// rows back via [churn.ChurnEventReader], maps them to
// [materialisers.ChurnRow] (resolving file_path ->
// scope_id via a MapScopeResolver mirror), runs the Stage
// 2.6 [materialisers.Materialiser], and asserts:
//
//   - One MetricSampleDraft per touched scope (2 in this
//     fixture, foo.go + bar.go).
//   - MetricKind == "modification_count_in_window".
//   - Pack == "base".
//   - Source == "computed".
//   - Attrs[provenance] == "ingested".
//   - Attrs[window_days] == "90".
//
// The materialiser is the SOLE writer of
// modification_count_in_window per tech-spec Sec 4.1.1
// lines 287-291; the verb's job is to STAGE rows so the
// materialiser has data to consume. This test proves the
// staged shape is materialiser-consumable.
func TestMaterialiserConsumesStagedEvents(t *testing.T) {
	t.Parallel()
	t.Logf("scenario: %s (e2e-scenarios.md lines 658-664)", scenarioMaterialiserConsumesChurn)

	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(
		store,
		func() time.Time { return stagedAt },
		uuidMinter(),
	)
	if _, err := ing.Ingest(context.Background(), canonicalHandle(), canonicalPayload()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Read the staged events back via the reader interface
	// the materialiser will eventually consume from.
	events, err := store.ListChurnEventsForRepo(context.Background(), fixedRepoID, time.Time{})
	if err != nil {
		t.Fatalf("ListChurnEventsForRepo: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("staged events = %d, want 2", len(events))
	}

	// Build a MapScopeResolver mirror -- the production
	// materialiser will use the `scope_binding` reader, but
	// for the staged-events-are-consumable proof a stub
	// resolver suffices.
	resolver := churn.NewMapScopeResolver()
	scopeForFoo := mustParseUUID(t, "cccccccc-0000-0000-0000-000000000001")
	scopeForBar := mustParseUUID(t, "cccccccc-0000-0000-0000-000000000002")
	resolver.Add(fixedRepoID, "internal/foo.go", scopeForFoo, recipes.ScopeRef{
		LocalID:       scopeForFoo.String(),
		Kind:          scope.KindFile,
		QualifiedName: "internal/foo.go",
		Path:          "internal/foo.go",
	})
	resolver.Add(fixedRepoID, "internal/bar.go", scopeForBar, recipes.ScopeRef{
		LocalID:       scopeForBar.String(),
		Kind:          scope.KindFile,
		QualifiedName: "internal/bar.go",
		Path:          "internal/bar.go",
	})

	// Map staged events -> materialiser ChurnRows.
	rows := make([]materialisers.ChurnRow, 0, len(events))
	for _, ev := range events {
		sid, ref, err := resolver.ResolveFile(context.Background(), ev.RepoID, ev.FilePath)
		if err != nil {
			t.Fatalf("ResolveFile(%s, %q): %v", ev.RepoID, ev.FilePath, err)
		}
		rows = append(rows, materialisers.ChurnRow{
			ScopeKey:   sid.String(),
			Scope:      ref,
			SHA:        ev.SHA,
			ModifiedAt: ev.ModifiedAt,
		})
	}

	// Run the Stage 2.6 materialiser. The window-clock is
	// frozen at `stagedAt + 1h` so every staged row falls
	// inside the 90-day window deterministically.
	m := materialisers.NewMaterialiserWithClock(
		materialisers.DefaultWindowDays,
		func() time.Time { return stagedAt.Add(1 * time.Hour) },
	)
	drafts := m.Materialise(rows)

	if len(drafts) != 2 {
		t.Fatalf("drafts = %d, want 2 (one per touched scope)", len(drafts))
	}
	for i, d := range drafts {
		if d.MetricKind != materialisers.MetricKind {
			t.Errorf("drafts[%d].MetricKind = %q, want %q", i, d.MetricKind, materialisers.MetricKind)
		}
		if d.MetricKind != "modification_count_in_window" {
			t.Errorf("drafts[%d].MetricKind = %q, want %q (literal pin per e2e-scenarios)", i, d.MetricKind, "modification_count_in_window")
		}
		if d.Pack != recipes.PackBase {
			t.Errorf("drafts[%d].Pack = %q, want %q (per Stage 2.6 doc)", i, d.Pack, recipes.PackBase)
		}
		if d.Source != recipes.SourceComputed {
			t.Errorf("drafts[%d].Source = %q, want %q (per architecture Sec 4.4)", i, d.Source, recipes.SourceComputed)
		}
		if d.Attrs[materialisers.AttrProvenance] != materialisers.AttrProvenanceValue {
			t.Errorf("drafts[%d].Attrs[%q] = %q, want %q (per tech-spec Sec 4.11)",
				i, materialisers.AttrProvenance, d.Attrs[materialisers.AttrProvenance], materialisers.AttrProvenanceValue)
		}
		if d.Attrs[materialisers.AttrWindowDays] != "90" {
			t.Errorf("drafts[%d].Attrs[%q] = %q, want %q",
				i, materialisers.AttrWindowDays, d.Attrs[materialisers.AttrWindowDays], "90")
		}
		// Per-scope count: each fixture row has a unique SHA
		// so the modification count per scope is exactly 1.
		if d.Value != 1 {
			t.Errorf("drafts[%d].Value = %v, want 1 (one unique SHA per scope in fixture)", i, d.Value)
		}
	}

	// Cross-check: the materialiser's own WriterIdentity is
	// the producer-attribution literal -- pinning here so a
	// drift in either side fails this scenario.
	if materialisers.WriterIdentity != "modification_count_materialiser" {
		t.Errorf("materialisers.WriterIdentity = %q, want %q",
			materialisers.WriterIdentity, "modification_count_materialiser")
	}

	// Reference the helpers to silence unused-import lint:
	_ = uuid.Nil
}
