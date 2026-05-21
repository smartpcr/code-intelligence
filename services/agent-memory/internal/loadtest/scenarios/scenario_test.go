package scenarios_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/scenarios"
)

// fakeAgentClient is a deterministic in-memory AgentClient
// every scenario test wires.
type fakeAgentClient struct {
	mu sync.Mutex

	recallResp     scenarios.RecallResponse
	recallErr      error
	observeResp    scenarios.ObserveResponse
	observeErr     error
	expandResp     scenarios.ExpandResponse
	expandErr      error
	summarizeResp  scenarios.SummarizeResponse
	summarizeErr   error
	recallCalls    int
	observeCalls   int
	expandCalls    int
	summarizeCalls int
	latency        time.Duration

	lastRecall    scenarios.RecallRequest
	lastObserve   scenarios.ObserveRequest
	lastExpand    scenarios.ExpandRequest
	lastSummarize scenarios.SummarizeRequest
}

func (f *fakeAgentClient) Recall(_ context.Context, req scenarios.RecallRequest) (scenarios.RecallResponse, error) {
	f.mu.Lock()
	f.recallCalls++
	f.lastRecall = req
	lat := f.latency
	resp := f.recallResp
	err := f.recallErr
	f.mu.Unlock()
	if lat > 0 {
		time.Sleep(lat)
	}
	return resp, err
}

func (f *fakeAgentClient) Observe(_ context.Context, req scenarios.ObserveRequest) (scenarios.ObserveResponse, error) {
	f.mu.Lock()
	f.observeCalls++
	f.lastObserve = req
	lat := f.latency
	resp := f.observeResp
	err := f.observeErr
	f.mu.Unlock()
	if lat > 0 {
		time.Sleep(lat)
	}
	return resp, err
}

func (f *fakeAgentClient) Expand(_ context.Context, req scenarios.ExpandRequest) (scenarios.ExpandResponse, error) {
	f.mu.Lock()
	f.expandCalls++
	f.lastExpand = req
	lat := f.latency
	resp := f.expandResp
	err := f.expandErr
	f.mu.Unlock()
	if lat > 0 {
		time.Sleep(lat)
	}
	return resp, err
}

func (f *fakeAgentClient) Summarize(_ context.Context, req scenarios.SummarizeRequest) (scenarios.SummarizeResponse, error) {
	f.mu.Lock()
	f.summarizeCalls++
	f.lastSummarize = req
	lat := f.latency
	resp := f.summarizeResp
	err := f.summarizeErr
	f.mu.Unlock()
	if lat > 0 {
		time.Sleep(lat)
	}
	return resp, err
}

type fakeMgmtClient struct {
	calls atomic.Uint64
	resp  scenarios.IngestSpansResponse
	err   error
	last  scenarios.IngestSpansRequest
	mu    sync.Mutex
}

func (f *fakeMgmtClient) IngestSpans(_ context.Context, req scenarios.IngestSpansRequest) (scenarios.IngestSpansResponse, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.last = req
	f.mu.Unlock()
	return f.resp, f.err
}

type fixedRNG struct{ next int }

func (r *fixedRNG) Intn(_ int) int { return r.next }

func TestRecallScenario_ComputesRankAndConceptHit(t *testing.T) {
	client := &fakeAgentClient{
		recallResp: scenarios.RecallResponse{
			ContextID:  "ctx-1",
			NodeIDs:    []string{"n0", "n1", "n2", "expected", "n4"},
			ConceptIDs: []string{"c0", "c1", "c2"},
		},
	}
	s := &scenarios.RecallScenario{
		Client: client,
		RepoID: "repo-x",
		K:      20,
		Queries: []scenarios.RecallQuery{
			{Query: "q1", ExpectedNodeID: "expected", ExpectedConceptIDs: []string{"c1"}},
		},
	}
	sample := s.Execute(context.Background(), &fixedRNG{next: 0})
	if sample.Err != nil {
		t.Fatalf("Execute returned err: %v", sample.Err)
	}
	if sample.Verb != "agent.recall" {
		t.Errorf("Verb: want agent.recall, got %s", sample.Verb)
	}
	if sample.RecallRank != 4 {
		t.Errorf("RecallRank: want 4, got %d", sample.RecallRank)
	}
	if !sample.ConceptHit {
		t.Error("ConceptHit should be true (c1 present)")
	}
	if !sample.ConceptHitMeasured {
		t.Error("ConceptHitMeasured should be true")
	}
	if sample.Started.IsZero() || sample.Finished.IsZero() {
		t.Error("Started/Finished must be stamped")
	}
}

func TestRecallScenario_NodeNotFound_RankZero(t *testing.T) {
	client := &fakeAgentClient{recallResp: scenarios.RecallResponse{NodeIDs: []string{"a", "b"}}}
	s := &scenarios.RecallScenario{
		Client:  client,
		Queries: []scenarios.RecallQuery{{Query: "x", ExpectedNodeID: "missing"}},
	}
	sample := s.Execute(context.Background(), &fixedRNG{})
	if sample.RecallRank != 0 {
		t.Errorf("RecallRank for missing node: want 0, got %d", sample.RecallRank)
	}
	if sample.ConceptHitMeasured {
		t.Error("ConceptHitMeasured should be false (no expected concepts)")
	}
}

func TestRecallScenario_FallbacksWhenQueriesEmpty(t *testing.T) {
	client := &fakeAgentClient{}
	s := &scenarios.RecallScenario{Client: client}
	sample := s.Execute(context.Background(), &fixedRNG{})
	if sample.Err != nil {
		t.Fatalf("fallback path err: %v", sample.Err)
	}
	if client.lastRecall.Query == "" {
		t.Error("Recall should have been called with synthetic query")
	}
	if client.lastRecall.K != 20 {
		t.Errorf("K default: want 20, got %d", client.lastRecall.K)
	}
}

func TestRecallScenario_ErrorPropagates(t *testing.T) {
	want := errors.New("boom")
	client := &fakeAgentClient{recallErr: want}
	s := &scenarios.RecallScenario{Client: client, Queries: []scenarios.RecallQuery{{Query: "q"}}}
	sample := s.Execute(context.Background(), &fixedRNG{})
	if !errors.Is(sample.Err, want) {
		t.Errorf("Err: want %v, got %v", want, sample.Err)
	}
	if sample.Finished.IsZero() || sample.Started.IsZero() {
		t.Error("timestamps must be stamped even on err so latency is reported")
	}
}

func TestObserveScenario_RotatesContextIDsAndOutcomes(t *testing.T) {
	client := &fakeAgentClient{observeResp: scenarios.ObserveResponse{EpisodeID: "ep-1"}}
	s := &scenarios.ObserveScenario{
		Client:              client,
		RepoID:              "r",
		SyntheticContextIDs: []string{"c-a", "c-b"},
		OutcomeRotation:     []string{"success", "failure"},
	}
	contexts := map[string]int{}
	outcomes := map[string]int{}
	for i := 0; i < 4; i++ {
		s.Execute(context.Background(), nil)
		contexts[client.lastObserve.ContextID]++
		outcomes[client.lastObserve.Outcome]++
	}
	if contexts["c-a"] != 2 || contexts["c-b"] != 2 {
		t.Errorf("context rotation uneven: %v", contexts)
	}
	if outcomes["success"] != 2 || outcomes["failure"] != 2 {
		t.Errorf("outcome rotation uneven: %v", outcomes)
	}
}

func TestObserveScenario_FallbackSyntheticContextID(t *testing.T) {
	client := &fakeAgentClient{}
	s := &scenarios.ObserveScenario{Client: client, RepoID: "r"}
	s.Execute(context.Background(), nil)
	if client.lastObserve.ContextID == "" {
		t.Error("Observe should have minted a synthetic context_id")
	}
	if !strings.HasPrefix(client.lastObserve.ContextID, "00000000-0000-4000-8000-") {
		t.Errorf("synthetic context_id shape unexpected: %q", client.lastObserve.ContextID)
	}
}

func TestObserveScenario_StampsActionJSON(t *testing.T) {
	client := &fakeAgentClient{}
	s := &scenarios.ObserveScenario{Client: client, RepoID: "r"}
	s.Execute(context.Background(), nil)
	if !strings.Contains(string(client.lastObserve.ActionJSON), `"kind":"calibration"`) {
		t.Errorf("ActionJSON missing kind=calibration: %s", string(client.lastObserve.ActionJSON))
	}
}

func TestCallChainQueryScenario_RotatesDirectionAndDepth(t *testing.T) {
	client := &fakeAgentClient{expandResp: scenarios.ExpandResponse{NodeIDs: []string{"n"}}}
	s := &scenarios.CallChainQueryScenario{
		Client:      client,
		RepoID:      "r",
		SeedNodeIDs: []string{"seed-a", "seed-b"},
		MaxDepth:    3,
		MaxNodes:    100,
		MaxEdges:    200,
	}
	directions := map[string]int{}
	depths := map[int32]int{}
	for i := 0; i < 6; i++ {
		s.Execute(context.Background(), nil)
		directions[client.lastExpand.Direction]++
		depths[client.lastExpand.Depth]++
	}
	if directions["callees"] != 3 || directions["callers"] != 3 {
		t.Errorf("direction rotation uneven: %v", directions)
	}
	if depths[1] != 2 || depths[2] != 2 || depths[3] != 2 {
		t.Errorf("depth rotation uneven: %v", depths)
	}
}

func TestCallChainQueryScenario_VerbIsExpand(t *testing.T) {
	s := &scenarios.CallChainQueryScenario{}
	if s.Verb() != "agent.expand" {
		t.Errorf("CallChainQueryScenario.Verb() = %q, want agent.expand", s.Verb())
	}
}

func TestSummarizeScenario_AlternatesNodeAndConcept(t *testing.T) {
	client := &fakeAgentClient{summarizeResp: scenarios.SummarizeResponse{TargetKind: "node"}}
	s := &scenarios.SummarizeScenario{
		Client:     client,
		RepoID:     "r",
		NodeIDs:    []string{"n1"},
		ConceptIDs: []string{"c1"},
		MaxTokens:  256,
	}
	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		s.Execute(context.Background(), nil)
		switch {
		case client.lastSummarize.NodeID != "":
			counts["node"]++
		case client.lastSummarize.ConceptID != "":
			counts["concept"]++
		}
	}
	if counts["node"] != 2 || counts["concept"] != 2 {
		t.Errorf("summarize rotation uneven: %v", counts)
	}
}

func TestSummarizeScenario_FallbackSyntheticNode(t *testing.T) {
	client := &fakeAgentClient{}
	s := &scenarios.SummarizeScenario{Client: client, RepoID: "r"}
	s.Execute(context.Background(), nil)
	if client.lastSummarize.NodeID == "" && client.lastSummarize.ConceptID == "" {
		t.Error("Summarize fallback should have minted a target id")
	}
}

func TestGraphIngestScenario_DefaultEncoderShape(t *testing.T) {
	client := &fakeMgmtClient{resp: scenarios.IngestSpansResponse{Accepted: 50}}
	s := &scenarios.GraphIngestScenario{Client: client, RepoID: "repo-x"}
	sample := s.Execute(context.Background(), nil)
	if sample.Err != nil {
		t.Fatalf("Execute: %v", sample.Err)
	}
	if !strings.Contains(string(client.last.BatchJSON), `"resourceSpans"`) {
		t.Error("default encoder should emit resourceSpans envelope")
	}
	if !strings.Contains(string(client.last.BatchJSON), `"repo-x"`) {
		t.Error("default encoder should embed repo_id")
	}
}

func TestGraphIngestScenario_RespectsSpansCap(t *testing.T) {
	client := &fakeMgmtClient{}
	s := &scenarios.GraphIngestScenario{Client: client, RepoID: "r", SpansPerBatch: 10_000}
	s.Execute(context.Background(), nil)
	// Hand-rolled encoder caps at 1000 spans/batch; verify by
	// counting span entries via a substring count.
	count := strings.Count(string(client.last.BatchJSON), `"name":"calibration.span"`)
	if count != 1000 {
		t.Errorf("SpansPerBatch cap: want 1000, got %d", count)
	}
}

func TestGraphIngestScenario_CustomEncoder(t *testing.T) {
	client := &fakeMgmtClient{}
	s := &scenarios.GraphIngestScenario{
		Client: client,
		RepoID: "r",
		BatchEncoder: func(repo string, n int, tick uint64) []byte {
			return []byte("CUSTOM:" + repo)
		},
	}
	s.Execute(context.Background(), nil)
	if string(client.last.BatchJSON) != "CUSTOM:r" {
		t.Errorf("custom encoder ignored: %s", string(client.last.BatchJSON))
	}
}

// TestGraphIngestScenario_ThreadsDegradedFlag pins the wire
// contract that the mgmt-api's `degraded=true` response flag
// (mirrors `internal/mgmtapi.SpanIngestResponse.Degraded`) MUST
// land on `Sample.Degraded` so the harness's
// degraded-responses note in the artifact reflects mgmt
// backpressure. An earlier iter discarded the response with
// `_ = resp`, which meant degraded mgmt responses were silently
// indistinguishable from clean accepts in the calibration
// baseline.
func TestGraphIngestScenario_ThreadsDegradedFlag(t *testing.T) {
	client := &fakeMgmtClient{
		resp: scenarios.IngestSpansResponse{
			Accepted:       50,
			Degraded:       true,
			DegradedReason: "writer_backpressure",
		},
	}
	s := &scenarios.GraphIngestScenario{Client: client, RepoID: "r"}
	sample := s.Execute(context.Background(), nil)
	if sample.Err != nil {
		t.Fatalf("Execute: %v", sample.Err)
	}
	if !sample.Degraded {
		t.Errorf("Sample.Degraded dropped: want true, got false")
	}
}

func TestSample_LatencyZeroWhenUnset(t *testing.T) {
	var s scenarios.Sample
	if s.Latency() != 0 {
		t.Errorf("Latency on empty sample: want 0, got %v", s.Latency())
	}
}

func TestSample_LatencyComputed(t *testing.T) {
	now := time.Now()
	s := scenarios.Sample{Started: now, Finished: now.Add(50 * time.Millisecond)}
	if got := s.Latency(); got != 50*time.Millisecond {
		t.Errorf("Latency: want 50ms, got %v", got)
	}
}

func TestQueriesFromLabeled_PreservesAllFields(t *testing.T) {
	src := []struct {
		Query              string
		ExpectedNodeID     string
		ExpectedConceptIDs []string
		Kinds              []string
	}{
		{Query: "q1", ExpectedNodeID: "n1", ExpectedConceptIDs: []string{"c1"}, Kinds: []string{"method"}},
	}
	out := scenarios.QueriesFromLabeled(src)
	if len(out) != 1 {
		t.Fatalf("len: want 1, got %d", len(out))
	}
	if out[0].Query != "q1" || out[0].ExpectedNodeID != "n1" {
		t.Errorf("conversion dropped fields: %+v", out[0])
	}
}
