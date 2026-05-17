package migrations

import (
	"strings"
	"testing"
)

// TestAll_parsesEveryEmbeddedFile confirms every .sql file
// embedded under //go:embed is parseable and emits a non-empty
// up body. It also asserts the lexicographic sort produces the
// implementation-plan.md Stage 1.2 + 1.3 + 1.4 + 2.2 + 3.4 + 3.5
// order (0001 .. 0006a then 0006b then 0007 .. 0014 then
// 0015 .. 0018).
func TestAll_parsesEveryEmbeddedFile(t *testing.T) {
	t.Parallel()
	all, err := All()
	if err != nil {
		t.Fatalf("All() returned error: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("All() returned zero migrations -- expected the Stage 1.2 + 1.3 + 1.4 set")
	}
	wantVersions := []string{
		// Stage 1.2 structural set.
		"0001", "0002", "0003", "0004", "0005", "0006", "0006a",
		// Stage 3.4 delta handler — ingest_jobs.affected_node_count column.
		"0006b",
		// Stage 1.3 episodic + concept set.
		"0007", "0008", "0009", "0010", "0011", "0012", "0013", "0014",
		// Stage 1.4 embedding-publish + role-grants set.
		"0015", "0016",
		// Stage 2.2 reader-role grant.
		"0017",
		// Stage 3.5 webhook receiver per-repo secret table.
		"0018",
		// Stage 4.2 Span Ingestor: degraded-state + solo-method aggregate.
		"0019", "0020",
		// Stage 6.1 Consolidator iter-4: durable candidate-support staging
		// for sub-threshold accumulation across ticks (see migration header).
		"0021",
	}
	if len(all) != len(wantVersions) {
		t.Fatalf("All() returned %d migrations, want %d (Stage 1.2 + 1.3 + 1.4 + 2.2 + 3.4 + 3.5 + 4.2 + 6.1 set)",
			len(all), len(wantVersions))
	}
	for i, w := range wantVersions {
		if all[i].Version != w {
			t.Errorf("migration[%d].Version = %q, want %q", i, all[i].Version, w)
		}
		if strings.TrimSpace(all[i].Up) == "" {
			t.Errorf("migration[%d] (%s) has empty up body", i, all[i].Filename)
		}
		if strings.TrimSpace(all[i].Down) == "" {
			t.Errorf("migration[%d] (%s) has empty down body -- round-trip requires both halves",
				i, all[i].Filename)
		}
	}
}

// TestAll_sortsInLexicographicOrder pins the apply order. The
// "0006a" letter suffix sorts after "0006" by plain string
// comparison; this is the single property the runner relies on
// for ordering, so we lock it in.
func TestAll_sortsInLexicographicOrder(t *testing.T) {
	t.Parallel()
	all, err := All()
	if err != nil {
		t.Fatalf("All() returned error: %v", err)
	}
	for i := 1; i < len(all); i++ {
		if !(all[i-1].Version < all[i].Version) {
			t.Errorf("migration order is not strictly ascending at index %d: %q then %q",
				i, all[i-1].Version, all[i].Version)
		}
	}
}

// TestSplitUpDown_basicSentinels exercises the parser directly so
// regressions in the marker matcher get caught even if every
// shipped .sql file accidentally happens to parse the same way.
func TestSplitUpDown_basicSentinels(t *testing.T) {
	t.Parallel()
	body := `
-- file header comment
-- migrate:up
CREATE TABLE x (id int);
-- migrate:down
DROP TABLE x;
`
	up, down := splitUpDown(body)
	if !strings.Contains(up, "CREATE TABLE x") {
		t.Errorf("up section missing CREATE TABLE: %q", up)
	}
	if strings.Contains(up, "DROP TABLE") {
		t.Errorf("up section leaked into down body: %q", up)
	}
	if !strings.Contains(down, "DROP TABLE x") {
		t.Errorf("down section missing DROP TABLE: %q", down)
	}
}

// TestSplitUpDown_preambleIgnored makes sure file-level comments
// that appear BEFORE the up marker do not bleed into either
// half. The Stage 1.2 .sql files all open with a multi-line
// header explaining the migration, and that header must never
// reach the database executor.
func TestSplitUpDown_preambleIgnored(t *testing.T) {
	t.Parallel()
	body := `-- preamble line 1
-- preamble line 2
-- migrate:up
SELECT 1;
-- migrate:down
SELECT 2;
`
	up, down := splitUpDown(body)
	if strings.Contains(up, "preamble") {
		t.Errorf("preamble leaked into up body: %q", up)
	}
	if strings.Contains(down, "preamble") {
		t.Errorf("preamble leaked into down body: %q", down)
	}
	if !strings.Contains(up, "SELECT 1") {
		t.Errorf("up body missing expected statement: %q", up)
	}
	if !strings.Contains(down, "SELECT 2") {
		t.Errorf("down body missing expected statement: %q", down)
	}
}

// TestStripTopLevelTxn_dropsSoloMarkers verifies that bare
// BEGIN/COMMIT/ROLLBACK lines are removed but anything else is
// left intact. The Migrator wraps each migration in its own
// transaction; the in-file markers exist for psql ergonomics and
// must not double-nest.
func TestStripTopLevelTxn_dropsSoloMarkers(t *testing.T) {
	t.Parallel()
	in := `BEGIN;
CREATE TABLE z (id int);
   COMMIT  ;
INSERT INTO z VALUES (1);
ROLLBACK;
`
	out := stripTopLevelTxn(in)
	for _, kw := range []string{"BEGIN", "COMMIT", "ROLLBACK"} {
		if strings.Contains(out, kw+";") {
			t.Errorf("stripped output still contains %s;:\n%s", kw, out)
		}
	}
	if !strings.Contains(out, "CREATE TABLE z") {
		t.Errorf("non-marker statement was stripped: %q", out)
	}
	if !strings.Contains(out, "INSERT INTO z") {
		t.Errorf("non-marker statement was stripped: %q", out)
	}
}

// TestParse_rejectsBadFilenames keeps the filename grammar
// enforced. The runner's apply order depends on the leading
// numeric token; a malformed filename must surface as a parse
// error rather than silently sorting to position zero.
func TestParse_rejectsBadFilenames(t *testing.T) {
	t.Parallel()
	if _, err := parse("oops.sql", "-- migrate:up\nSELECT 1;\n-- migrate:down\nSELECT 2;\n"); err == nil {
		t.Fatal("parse accepted a filename without a numeric prefix")
	}
}

// TestParse_rejectsMissingUpBlock guards against an authoring
// mistake where someone forgets the up marker entirely -- the
// runner would otherwise apply a no-op and journal a row that
// can never be reverted.
func TestParse_rejectsMissingUpBlock(t *testing.T) {
	t.Parallel()
	if _, err := parse("0099_no_up.sql", "-- file header\n"); err == nil {
		t.Fatal("parse accepted a file with no -- migrate:up block")
	}
}

// TestAll_filenamesMatchPlannedSet pins the literal filenames
// implementation-plan.md Stages 1.2 + 1.3 + 1.4 call out. If a
// future stage renames any of these (e.g. drops the "0006a"
// letter suffix), this test fails loudly so the rename is
// intentional.
func TestAll_filenamesMatchPlannedSet(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		// Stage 1.2 structural set.
		"0001_enums.sql":             true,
		"0002_repo_commit.sql":       true,
		"0003_node_edge.sql":         true,
		"0004_retirements.sql":       true,
		"0005_trace_observation.sql": true,
		"0006_repo_event.sql":        true,
		"0006a_ingest_jobs.sql":      true,
		// Stage 3.4 delta handler — adds the affected_node_count
		// column to ingest_jobs so the publish-retry path can
		// monotonically persist the per-run high-water mark.
		"0006b_ingest_jobs_affected_node_count.sql": true,
		// Stage 1.3 episodic + concept set.
		"0007_episode.sql":                   true,
		"0008_episode_update.sql":            true,
		"0009_observation.sql":               true,
		"0010_recall_context_log.sql":        true,
		"0011_concept.sql":                   true,
		"0012_run_tables.sql":                true,
		"0013_synthetic_positive_unique.sql": true,
		"0014_pg_partman_setup.sql":          true,
		// Stage 1.4 embedding-publish + role-grants set.
		"0015_embedding_publish.sql": true,
		"0016_roles_grants.sql":      true,
		// Stage 2.2 reader-role grant.
		"0017_reader_role.sql": true,
		// Stage 3.5 per-repo webhook secret table.
		"0018_repo_webhook_secret.sql": true,
		// Stage 4.2 Span Ingestor cross-process backpressure + root-span aggregate.
		"0019_repo_health.sql":              true,
		"0020_method_solo_observation.sql":  true,
		// Stage 6.1 Consolidator iter-4 durable candidate-support staging.
		"0021_concept_candidate.sql": true,
	}
	all, err := All()
	if err != nil {
		t.Fatalf("All() returned error: %v", err)
	}
	got := map[string]bool{}
	for _, m := range all {
		got[m.Filename] = true
	}
	for f := range want {
		if !got[f] {
			t.Errorf("planned migration file missing from embed.FS: %s", f)
		}
	}
	for f := range got {
		if !want[f] {
			t.Errorf("unexpected migration file in embed.FS: %s", f)
		}
	}
}
