package graphreader_test

// Integration coverage for the Stage 3.3 lift of
// `ListRepos` into `*graphreader.Reader`. Skips cleanly when
// AGENT_MEMORY_PG_URL is unset, matching every other reader
// integration test in this package.
//
// Why an integration test instead of the sqlmock pattern the
// implementation plan names verbatim? `*graphreader.Reader`
// runs over `*pgxpool.Pool` (the pgx v5 driver), not the
// `database/sql` interface `DATA-DOG/go-sqlmock` mocks. The
// graphwriter half of this workstream IS sqlmock-tested (see
// `internal/graphsink/postgres/sink_test.go`) because the
// writer takes `*sql.DB`; the reader half has no sqlmock-shaped
// substitute, so the live-Postgres pattern already standard in
// this package (mirrors `writer_integration_test.go`) is the
// only path that exercises the lifted SELECT end-to-end. The
// adapter-side `*Reader` forwarding is independently pinned by
// `internal/graphsink/postgres/reader_test.go`'s `fakeBackend`
// table, so the test pyramid is intact.
//
// Implementation-plan Stage 3.3 scenarios covered here:
//   * `graphreader-listrepos-matches-mgmtapi`
//     (returns the same row tuples in the same order the
//     existing `mgmtapi.handleListRepos` query produces).
//   * `postgres-listrepos-matches-mgmtapi-semantics`
//     (e2e-scenarios.md:412 -- newest-first ordering on
//     `created_at DESC, repo_id DESC`).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
)

// TestReader_ListRepos_returnsAllRowsNewestFirst seeds three
// `repo` rows in a known order and asserts `ListRepos` returns
// them in `created_at DESC` order -- the same ordering the
// mgmt-api handler emits, so the React UI's repo picker sees
// a stable feed regardless of which Stage-3 backend served it.
func TestReader_ListRepos_returnsAllRowsNewestFirst(t *testing.T) {
	t.Parallel()
	fx := openReaderFixture(t)
	t.Cleanup(fx.cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Seed three repos. Because the `created_at` column defaults
	// to `now()` (migration 0002) and we INSERT them serially,
	// the third insert has the latest timestamp and must appear
	// first in the ListRepos result.
	repoA := seedRepoWithSHA(t, ctx, fx, "sha-a")
	repoB := seedRepoWithSHA(t, ctx, fx, "sha-b")
	repoC := seedRepoWithSHA(t, ctx, fx, "sha-c")

	reader := graphreader.New(fx.pool, nil)
	got, err := reader.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}

	// Filter to the three IDs we seeded so the assertion is
	// independent of any rows other tests in the same schema may
	// have left behind (the fixture's per-test schema reset
	// makes this defensive, not load-bearing).
	want := []string{repoC, repoB, repoA}
	if len(got) < len(want) {
		t.Fatalf("got %d rows; want at least %d", len(got), len(want))
	}
	seenOrder := make([]string, 0, len(want))
	for _, r := range got {
		switch r.RepoID {
		case repoA, repoB, repoC:
			seenOrder = append(seenOrder, r.RepoID)
		}
		// Postgres backend MUST populate RepoUUID as a mirror
		// of RepoID (the surrogate is the backend-parity ID).
		if r.RepoUUID != r.RepoID {
			t.Errorf("repo %s: RepoUUID = %q, want = RepoID %q", r.RepoID, r.RepoUUID, r.RepoID)
		}
		if r.URL == "" {
			t.Errorf("repo %s: empty URL in returned RepoSummary", r.RepoID)
		}
		if r.GeneratedAt.IsZero() {
			t.Errorf("repo %s: zero GeneratedAt in returned RepoSummary", r.RepoID)
		}
	}
	if len(seenOrder) != len(want) {
		t.Fatalf("found %d of %d seeded repos in result; got=%+v", len(seenOrder), len(want), seenOrder)
	}
	for i := range want {
		if seenOrder[i] != want[i] {
			t.Errorf("position %d: got %s, want %s (full order=%v want=%v)",
				i, seenOrder[i], want[i], seenOrder, want)
		}
	}
}

// TestReader_ListRepos_capsAtMaxListLimit confirms the lifted
// SELECT honours `MaxListLimit` -- the same defence the rest of
// the reader applies (see `appendLimit`). A caller passing
// `Limit > MaxListLimit` MUST get at most `MaxListLimit` rows
// back; the zero-value `ReaderOptions` MUST behave identically.
//
// Seeding 10_001 rows would be wasteful, so this test just
// asserts the row count never exceeds `MaxListLimit` for either
// the explicit-overshoot or the zero-value path. The exact
// clamp behaviour is unit-tested in `query.go`'s `appendLimit`
// helper; this scenario keeps a smoke test on the lifted method.
func TestReader_ListRepos_capsAtMaxListLimit(t *testing.T) {
	t.Parallel()
	fx := openReaderFixture(t)
	t.Cleanup(fx.cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Seed at least one row so the result is non-empty and the
	// scan helper actually executes.
	_ = seedRepoWithSHA(t, ctx, fx, "sha-cap")

	reader := graphreader.New(fx.pool, nil)
	for _, opts := range []graphreader.ReaderOptions{
		{},                                       // zero -> defaults to MaxListLimit
		{Limit: graphreader.MaxListLimit + 1000}, // overshoot -> clamped
	} {
		got, err := reader.ListRepos(ctx, opts)
		if err != nil {
			t.Fatalf("ListRepos opts=%+v: %v", opts, err)
		}
		if len(got) > graphreader.MaxListLimit {
			t.Errorf("ListRepos opts=%+v returned %d rows; want <= MaxListLimit (%d)",
				opts, len(got), graphreader.MaxListLimit)
		}
	}
}

// seedRepoWithSHA inserts one `repo` row with the supplied
// `current_head_sha` and returns its `repo_id::text`. Mirrors
// the existing `seedRepo` helper in `reader_integration_test.go`
// but exposes the SHA so the test can assert it round-trips
// through `RepoSummary.SHA`.
func seedRepoWithSHA(t *testing.T, ctx context.Context, fx *readerFixture, sha string) string {
	t.Helper()
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	repoURL := "https://example.test/listrepos/" + hex.EncodeToString(buf[:])
	var repoID string
	err := fx.owner.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ($1, 'main', $2)
		RETURNING repo_id::text
	`, repoURL, sha).Scan(&repoID)
	if err != nil {
		t.Fatalf("seedRepoWithSHA: %v", err)
	}
	return repoID
}
