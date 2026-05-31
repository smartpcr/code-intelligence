package graphreader

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRepoSummaryJSONShape pins the wire contract for
// RepoSummary -- the single-source-of-truth value type the
// Stage 7.2 diagram envelope and the GET /v1/repos response
// marshal. Any change to a JSON tag is a coordinated breaking
// change against the React UI's TypeScript types and against
// every graphsink.Reader backend, so this test fails LOUDLY
// before any silent drift can ship.
func TestRepoSummaryJSONShape(t *testing.T) {
	ts := time.Date(2026, 5, 30, 21, 30, 0, 0, time.UTC)
	rs := RepoSummary{
		RepoID:      "github.com/owner/name",
		URL:         "https://github.com/owner/name",
		SHA:         "deadbeefcafe",
		GeneratedAt: ts,
		RepoUUID:    "11111111-2222-3333-4444-555555555555",
	}

	got, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("json.Marshal RepoSummary: %v", err)
	}

	const want = `{"repo_id":"github.com/owner/name","url":"https://github.com/owner/name","sha":"deadbeefcafe","generated_at":"2026-05-30T21:30:00Z","repo_uuid":"11111111-2222-3333-4444-555555555555"}`
	if string(got) != want {
		t.Fatalf("RepoSummary wire shape drifted:\n got: %s\nwant: %s", got, want)
	}

	// Field-by-field key existence check (defends against a
	// rename that happens to produce the same overall JSON for
	// the example above, e.g. swapping two strings).
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("json.Unmarshal RepoSummary: %v", err)
	}
	for _, k := range []string{"repo_id", "url", "sha", "generated_at", "repo_uuid"} {
		if _, ok := decoded[k]; !ok {
			t.Errorf("RepoSummary JSON missing required key %q (got keys: %v)", k, mapKeys(decoded))
		}
	}
}

// TestRepoSummaryJSONOmitsEmptyOptionals pins the omitempty
// contract on the two optional fields (SHA, RepoUUID). The
// SQLite / in-memory backends populate RepoID, URL, and
// GeneratedAt but leave SHA and RepoUUID empty for repos that
// were registered without a SHA yet or that have no surrogate
// key; the wire envelope MUST drop those fields rather than
// emit empty strings, so the React UI can distinguish
// "field not set" from "field is the empty string".
func TestRepoSummaryJSONOmitsEmptyOptionals(t *testing.T) {
	rs := RepoSummary{
		RepoID:      "github.com/owner/name",
		URL:         "https://github.com/owner/name",
		GeneratedAt: time.Date(2026, 5, 30, 21, 30, 0, 0, time.UTC),
	}
	got, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("json.Marshal RepoSummary: %v", err)
	}
	if strings.Contains(string(got), "\"sha\"") {
		t.Errorf("RepoSummary JSON should omit empty sha: got %s", got)
	}
	if strings.Contains(string(got), "\"repo_uuid\"") {
		t.Errorf("RepoSummary JSON should omit empty repo_uuid: got %s", got)
	}
	// repo_id, url, generated_at are required even when empty
	// -- they are the identity of the row.
	for _, k := range []string{"\"repo_id\"", "\"url\"", "\"generated_at\""} {
		if !strings.Contains(string(got), k) {
			t.Errorf("RepoSummary JSON missing required key %s: got %s", k, got)
		}
	}
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
