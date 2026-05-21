package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/calibration"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/scenarios"
)

func TestParseFlags_Defaults(t *testing.T) {
	t.Parallel()
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.profile != "nominal" {
		t.Errorf("default profile: want nominal, got %s", f.profile)
	}
	if f.maxInflight != 256 {
		t.Errorf("default maxInflight: want 256, got %d", f.maxInflight)
	}
	if f.spansPerBatch != 50 {
		t.Errorf("default spansPerBatch: want 50, got %d", f.spansPerBatch)
	}
	if f.repoID != "ca11ca11-0000-4000-8000-000000000001" {
		t.Errorf("default repo-id: want a fixed UUID matching the mgmt-api repo_id contract, got %q", f.repoID)
	}
}

func TestParseFlags_RejectsInvalidProfile(t *testing.T) {
	t.Parallel()
	_, err := parseFlags([]string{"--profile", "burst"})
	if err == nil {
		t.Fatal("expected error on unknown profile")
	}
	if !strings.Contains(err.Error(), "nominal|smoke") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestParseFlags_RejectsZeroInflight(t *testing.T) {
	t.Parallel()
	_, err := parseFlags([]string{"--max-inflight", "0"})
	if err == nil {
		t.Fatal("expected error on max-inflight=0")
	}
}

func TestParseFlags_AcceptsOverrides(t *testing.T) {
	t.Parallel()
	f, err := parseFlags([]string{
		"--profile", "smoke",
		"--duration", "100ms",
		"--artifact", "x.md",
		"--repo-id", "r",
		"--seed", "123",
		"--max-inflight", "8",
		"--spans-per-batch", "100",
		"--seeded-loc", "5000",
		"--no-tls",
		"--skip-mgmt",
		"--request-timeout", "2s",
		"--mgmt-ingest-path", "/v2/spans",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.profile != "smoke" || f.duration != 100*time.Millisecond || f.artifact != "x.md" ||
		f.repoID != "r" || f.seed != 123 || f.maxInflight != 8 || f.spansPerBatch != 100 ||
		f.seededFixtureLOC != 5000 || !f.disableTLS || !f.skipMgmt ||
		f.requestTimeout != 2*time.Second || f.mgmtIngestPath != "/v2/spans" {
		t.Errorf("flag values not propagated: %#v", f)
	}
}

func TestBuildConfig_SmokeProfile(t *testing.T) {
	t.Parallel()
	cfg := buildConfig(cliFlags{profile: "smoke", artifact: "z.md", repoID: "r", maxInflight: 4})
	if cfg.Profile.Name != "smoke" {
		t.Errorf("expected smoke profile, got %s", cfg.Profile.Name)
	}
	if cfg.ArtifactPath != "z.md" {
		t.Errorf("artifact not propagated: %s", cfg.ArtifactPath)
	}
}

func TestRun_EndToEnd_SmokeArtifactWritten(t *testing.T) {
	t.Parallel()
	// Spin a local httptest server that ack-202s every span
	// batch, accepting the harness's mgmt scenario without a
	// real mgmt-api binary.
	var hits atomic.Int64
	mgmt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"repo_id":"ca11ca11-0000-4000-8000-000000000001","accepted_spans":50,"degraded":false}`))
	}))
	defer mgmt.Close()

	dir := t.TempDir()
	artifact := filepath.Join(dir, "iter1.md")

	// Skip the agent.* surface (no gRPC server in this test);
	// the mgmt-only run is sufficient to exercise the end-to-
	// end harness wiring + artifact path.
	code := run([]string{
		"--profile", "smoke",
		"--duration", "200ms",
		"--artifact", artifact,
		"--mgmt-target", mgmt.URL,
		"--skip-agent",
		"--max-inflight", "8",
	}, &bytes.Buffer{})

	if code != exitOK {
		t.Fatalf("expected exitOK, got %d", code)
	}
	if hits.Load() == 0 {
		t.Error("expected mgmt server to receive at least one span batch")
	}
	body, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	for _, want := range []string{
		"Load-test calibration",
		"mgmt.ingest_spans",
		"labelled-query proxy",
		"profile: smoke",
		"**Status:** PASS",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("artifact missing %q. Body:\n%s", want, string(body))
		}
	}
}

func TestDecideExitCode_AbortedBeatsBreach(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rep  calibration.Report
		want int
	}{
		{
			name: "clean run",
			rep:  calibration.Report{},
			want: exitOK,
		},
		{
			name: "breach only",
			rep:  calibration.Report{BudgetBreaches: []string{"agent.recall"}},
			want: exitBreach,
		},
		{
			name: "aborted only",
			rep:  calibration.Report{Aborted: true, CompletionReason: "aborted-context-cancelled"},
			want: exitAborted,
		},
		{
			// Regression guard for iter-1 feedback item #5:
			// an aborted partial run with a synthetic breach
			// MUST exit aborted (3), NOT breach (1). A
			// partial-window percentile is not a baseline.
			name: "aborted AND breach",
			rep: calibration.Report{
				Aborted:          true,
				CompletionReason: "aborted-context-cancelled",
				BudgetBreaches:   []string{"agent.recall", "mgmt.ingest_spans"},
			},
			want: exitAborted,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideExitCode(tc.rep)
			if got != tc.want {
				t.Errorf("decideExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRun_NoScenariosEnabled_ReturnsFail(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run([]string{"--skip-agent", "--skip-mgmt", "--artifact", filepath.Join(t.TempDir(), "x.md"), "--duration", "10ms"}, &stderr)
	if code != exitFail {
		t.Fatalf("want exitFail, got %d. stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no scenarios enabled") {
		t.Errorf("unexpected stderr: %s", stderr.String())
	}
}

func TestRun_InvalidFlag_ReturnsFail(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run([]string{"--profile", "weird"}, &stderr)
	if code != exitFail {
		t.Fatalf("want exitFail, got %d", code)
	}
}

func TestMgmtClient_PostsBodyAndHeaders(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	var gotRepo string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = readAll(r)
		gotRepo = r.Header.Get("X-Mgmt-Repo-ID")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"repo_id":"ca11ca11-0000-4000-8000-000000000001","accepted_spans":7,"degraded":false}`))
	}))
	defer server.Close()

	c := newMgmtClient(server.URL, "/v1/spans", 5*time.Second)
	resp, err := c.IngestSpans(context.Background(), scenarios.IngestSpansRequest{
		RepoID:    "ca11ca11-0000-4000-8000-000000000001",
		BatchJSON: []byte(`{"hello":"world"}`),
	})
	if err != nil {
		t.Fatalf("IngestSpans: %v", err)
	}
	if resp.Accepted != 7 {
		t.Errorf("Accepted: want 7 (from accepted_spans), got %d", resp.Accepted)
	}
	if string(gotBody) != `{"hello":"world"}` {
		t.Errorf("body not forwarded: %s", string(gotBody))
	}
	if gotRepo != "ca11ca11-0000-4000-8000-000000000001" {
		t.Errorf("X-Mgmt-Repo-ID header not forwarded: %s", gotRepo)
	}
}

func TestMgmtClient_DoesNotSendDeprecatedHeader(t *testing.T) {
	t.Parallel()
	// Regression guard: an earlier iter sent the wrong header
	// name (`X-Agent-Memory-Repo-ID`); the mgmt-api only
	// recognises `X-Mgmt-Repo-ID`. Verify the old name is
	// gone so a re-introduction is caught at test time.
	var oldHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldHeader = r.Header.Get("X-Agent-Memory-Repo-ID")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted_spans":1}`))
	}))
	defer server.Close()

	c := newMgmtClient(server.URL, "/v1/spans", 5*time.Second)
	if _, err := c.IngestSpans(context.Background(), scenarios.IngestSpansRequest{
		RepoID:    "ca11ca11-0000-4000-8000-000000000001",
		BatchJSON: []byte("{}"),
	}); err != nil {
		t.Fatalf("IngestSpans: %v", err)
	}
	if oldHeader != "" {
		t.Errorf("deprecated X-Agent-Memory-Repo-ID still being sent: %q", oldHeader)
	}
}

func TestMgmtClient_PropagatesHTTPError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"bad batch"}`))
	}))
	defer server.Close()

	c := newMgmtClient(server.URL, "/v1/spans", 5*time.Second)
	_, err := c.IngestSpans(context.Background(), scenarios.IngestSpansRequest{
		BatchJSON: []byte("{}"),
	})
	if err == nil {
		t.Fatal("expected error on HTTP 422")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("missing status code in error: %v", err)
	}
}

// TestMgmtClient_ThreadsDegradedFlag pins the wire contract that
// the harness MUST propagate the mgmt-api's `degraded=true`
// response flag onto Sample.Degraded so the artifact's
// degraded-responses note reflects mgmt backpressure. Without
// this thread-through, mgmt.ingest_spans degraded responses
// would be silently treated as clean successes and the operator
// would never see backpressure in the calibration baseline.
func TestMgmtClient_ThreadsDegradedFlag(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"repo_id":"r","accepted_spans":3,"degraded":true,"degraded_reason":"writer_backpressure"}`))
	}))
	defer server.Close()

	c := newMgmtClient(server.URL, "/v1/spans", 5*time.Second)
	resp, err := c.IngestSpans(context.Background(), scenarios.IngestSpansRequest{
		RepoID:    "r",
		BatchJSON: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("IngestSpans: %v", err)
	}
	if !resp.Degraded {
		t.Errorf("Degraded flag dropped: want true, got false")
	}
	if resp.DegradedReason != "writer_backpressure" {
		t.Errorf("DegradedReason dropped: want %q, got %q",
			"writer_backpressure", resp.DegradedReason)
	}
}

// TestMgmtClient_RejectsMalformed2xx pins the contract that a
// 2xx response with a malformed JSON body is a verb failure —
// NOT a silent success. The earlier iter swallowed the
// json.Unmarshal error (`_ = json.Unmarshal(...)`) which meant
// a backend that 202-then-corrupts looked indistinguishable
// from a clean accept; the artifact's per-verb Failed count
// would never reflect the corruption.
func TestMgmtClient_RejectsMalformed2xx(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{this is not json`))
	}))
	defer server.Close()

	c := newMgmtClient(server.URL, "/v1/spans", 5*time.Second)
	_, err := c.IngestSpans(context.Background(), scenarios.IngestSpansRequest{
		BatchJSON: []byte("{}"),
	})
	if err == nil {
		t.Fatal("expected error on malformed 2xx body")
	}
	if !strings.Contains(err.Error(), "malformed 2xx") {
		t.Errorf("error message should call out the malformed-body contract: %v", err)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

func TestLoadLabeledQueriesJSON_EmptyPathReturnsNil(t *testing.T) {
	t.Parallel()
	queries, err := loadLabeledQueriesJSON("")
	if err != nil {
		t.Fatalf("empty path should not error: %v", err)
	}
	if queries != nil {
		t.Errorf("empty path should return nil, got %#v", queries)
	}
}

func TestLoadLabeledQueriesJSON_ParsesValidFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "labeled.json")
	body := `[
	  {
	    "query": "how do we hash a node id?",
	    "expected_node_id": "node:pkg/fingerprint/NodeFingerprint",
	    "expected_concept_ids": ["concept:fingerprint", "concept:hash"],
	    "kinds": ["method"]
	  },
	  {
	    "query": "list every span ingest path",
	    "expected_concept_ids": ["concept:ingestion"]
	  }
	]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	queries, err := loadLabeledQueriesJSON(path)
	if err != nil {
		t.Fatalf("loadLabeledQueriesJSON: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(queries))
	}
	if queries[0].Query != "how do we hash a node id?" ||
		queries[0].ExpectedNodeID != "node:pkg/fingerprint/NodeFingerprint" ||
		len(queries[0].ExpectedConceptIDs) != 2 ||
		len(queries[0].Kinds) != 1 || queries[0].Kinds[0] != "method" {
		t.Errorf("entry 0 wrong shape: %#v", queries[0])
	}
	if queries[1].Query != "list every span ingest path" ||
		queries[1].ExpectedNodeID != "" ||
		len(queries[1].ExpectedConceptIDs) != 1 {
		t.Errorf("entry 1 wrong shape: %#v", queries[1])
	}
}

func TestLoadLabeledQueriesJSON_RejectsMissingQuery(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`[{"expected_node_id":"n1"}]`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := loadLabeledQueriesJSON(path); err == nil {
		t.Fatal("expected error on empty query")
	}
}

func TestLoadLabeledQueriesJSON_RejectsMalformed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "junk.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := loadLabeledQueriesJSON(path); err == nil {
		t.Fatal("expected error on malformed json")
	}
}

func TestRun_EndToEnd_LoadsLabeledQueriesFromFile(t *testing.T) {
	t.Parallel()
	mgmt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted_spans":50,"degraded":false}`))
	}))
	defer mgmt.Close()

	dir := t.TempDir()
	artifact := filepath.Join(dir, "iter1.md")
	labeledPath := filepath.Join(dir, "labeled.json")
	if err := os.WriteFile(labeledPath, []byte(`[{"query":"q1","expected_node_id":"n1"}]`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Skip the agent.* surface so we don't need a gRPC server;
	// the labeled-queries loader runs regardless of whether
	// the recall scenario will actually consume them — the
	// goal here is to prove parseFlags → loadLabeledQueriesJSON
	// → cfg.LabeledQueries → cfg.Validate() is wired and the
	// run exits 0.
	code := run([]string{
		"--profile", "smoke",
		"--duration", "200ms",
		"--artifact", artifact,
		"--mgmt-target", mgmt.URL,
		"--skip-agent",
		"--labeled-queries", labeledPath,
		"--max-inflight", "8",
	}, &bytes.Buffer{})
	if code != exitOK {
		t.Fatalf("want exitOK, got %d", code)
	}
}

func TestRun_RejectsMissingLabeledQueriesFile(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := run([]string{
		"--profile", "smoke",
		"--duration", "10ms",
		"--artifact", filepath.Join(t.TempDir(), "x.md"),
		"--skip-agent",
		"--skip-mgmt", // also rejects via "no scenarios" but the labeled-file error must fire first
		"--labeled-queries", filepath.Join(t.TempDir(), "does-not-exist.json"),
	}, &stderr)
	if code != exitFail {
		t.Fatalf("want exitFail, got %d. stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "labeled-queries") {
		t.Errorf("expected stderr to reference labeled-queries: %s", stderr.String())
	}
}
