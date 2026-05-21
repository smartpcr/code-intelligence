package reliability_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

func TestNominalLoadProfile_Matches_Section_8_3_Envelope(t *testing.T) {
	p := reliability.NominalLoadProfile()
	if err := p.Validate(); err != nil {
		t.Fatalf("nominal profile failed Validate: %v", err)
	}
	if p.Name != "nominal" {
		t.Errorf("Name: want %q, got %q", "nominal", p.Name)
	}
	if p.DefaultDuration != 30*time.Minute {
		t.Errorf("DefaultDuration: want 30m, got %v", p.DefaultDuration)
	}
	if math.Abs(p.ErrorBudgetRatio-0.01) > 1e-9 {
		t.Errorf("ErrorBudgetRatio: want 0.01, got %v", p.ErrorBudgetRatio)
	}
	// Every verb in the §8.3 envelope must appear.
	want := map[string]struct {
		rps          float64
		slo95, slo99 float64
		metric       string
	}{
		reliability.VerbAgentRecall:     {50, 1.5, 4, "agent_recall_duration_seconds"},
		reliability.VerbAgentObserve:    {50, 0.4, 1.5, "agent_observe_duration_seconds"},
		reliability.VerbAgentExpand:     {20, 1.5, 4, "agent_expand_duration_seconds"},
		reliability.VerbAgentSummarize:  {5, 4, 10, "agent_summarize_duration_seconds"},
		reliability.VerbMgmtIngestSpans: {50.0 / 60.0, 2, 5, "mgmt_ingest_spans_batch_duration_seconds"},
	}
	for verb, exp := range want {
		v, ok := p.Verb(verb)
		if !ok {
			t.Errorf("nominal profile missing verb %q", verb)
			continue
		}
		if math.Abs(v.SustainedRPS-exp.rps) > 1e-9 {
			t.Errorf("%s: SustainedRPS want %v, got %v", verb, exp.rps, v.SustainedRPS)
		}
		if math.Abs(v.SLO95Seconds-exp.slo95) > 1e-9 {
			t.Errorf("%s: SLO95Seconds want %v, got %v", verb, exp.slo95, v.SLO95Seconds)
		}
		if math.Abs(v.SLO99Seconds-exp.slo99) > 1e-9 {
			t.Errorf("%s: SLO99Seconds want %v, got %v", verb, exp.slo99, v.SLO99Seconds)
		}
		if v.MetricName != exp.metric {
			t.Errorf("%s: MetricName want %q, got %q", verb, exp.metric, v.MetricName)
		}
	}
	if p.LearningQuality.K != 20 {
		t.Errorf("LearningQuality.K want 20, got %d", p.LearningQuality.K)
	}
	if p.LearningQuality.MaxMedianRankAtK != 5 {
		t.Errorf("LearningQuality.MaxMedianRankAtK want 5, got %d", p.LearningQuality.MaxMedianRankAtK)
	}
	if math.Abs(p.LearningQuality.MinConceptHitFractionAtK-0.25) > 1e-9 {
		t.Errorf("LearningQuality.MinConceptHitFractionAtK want 0.25, got %v", p.LearningQuality.MinConceptHitFractionAtK)
	}
}

func TestVerbProfile_Interval_And_PlannedRequests(t *testing.T) {
	v := reliability.VerbProfile{Verb: "agent.recall", SustainedRPS: 50, MetricName: "x", SLO95Seconds: 1, SLO99Seconds: 2}
	if got, want := v.Interval(), 20*time.Millisecond; got != want {
		t.Errorf("Interval at 50 RPS: want %v, got %v", want, got)
	}
	if got, want := v.PlannedRequests(time.Second), 50; got != want {
		t.Errorf("PlannedRequests for 1s: want %d, got %d", want, got)
	}
	if got, want := v.PlannedRequests(0), 0; got != want {
		t.Errorf("PlannedRequests for 0 duration: want %d, got %d", want, got)
	}
	v.SustainedRPS = 0
	if got := v.Interval(); got != 0 {
		t.Errorf("Interval at 0 RPS: want 0, got %v", got)
	}
}

func TestLoadProfile_Validate_Rejects_BadInput(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*reliability.LoadProfile)
		wantHits []string
	}{
		{
			name:     "empty name",
			mutate:   func(p *reliability.LoadProfile) { p.Name = "" },
			wantHits: []string{"Name is required"},
		},
		{
			name:     "empty verbs",
			mutate:   func(p *reliability.LoadProfile) { p.Verbs = nil },
			wantHits: []string{"Verbs must be non-empty"},
		},
		{
			name:     "zero duration",
			mutate:   func(p *reliability.LoadProfile) { p.DefaultDuration = 0 },
			wantHits: []string{"DefaultDuration must be positive"},
		},
		{
			name:     "negative error budget",
			mutate:   func(p *reliability.LoadProfile) { p.ErrorBudgetRatio = -0.1 },
			wantHits: []string{"ErrorBudgetRatio must be in"},
		},
		{
			name:     "duplicate verb",
			mutate:   func(p *reliability.LoadProfile) { p.Verbs = append(p.Verbs, p.Verbs[0]) },
			wantHits: []string{"duplicate verb"},
		},
		{
			name: "negative RPS",
			mutate: func(p *reliability.LoadProfile) {
				p.Verbs[0].SustainedRPS = -1
			},
			wantHits: []string{"SustainedRPS must be > 0"},
		},
		{
			name: "p99 < p95",
			mutate: func(p *reliability.LoadProfile) {
				p.Verbs[0].SLO95Seconds = 5
				p.Verbs[0].SLO99Seconds = 1
			},
			wantHits: []string{"SLO99Seconds"},
		},
		{
			name: "K = 0",
			mutate: func(p *reliability.LoadProfile) {
				p.LearningQuality.K = 0
			},
			wantHits: []string{"LearningQuality.K"},
		},
		{
			name: "concept-hit fraction > 1",
			mutate: func(p *reliability.LoadProfile) {
				p.LearningQuality.MinConceptHitFractionAtK = 1.5
			},
			wantHits: []string{"MinConceptHitFractionAtK"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := reliability.NominalLoadProfile()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatal("Validate returned nil, want error")
			}
			for _, want := range tc.wantHits {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing substring %q", err.Error(), want)
				}
			}
		})
	}
}

func TestSmokeProfile_FitsInSubSecond(t *testing.T) {
	p := reliability.SmokeProfile()
	if err := p.Validate(); err != nil {
		t.Fatalf("smoke profile failed Validate: %v", err)
	}
	if p.Name != "smoke" {
		t.Errorf("Name: want %q, got %q", "smoke", p.Name)
	}
	if p.DefaultDuration > time.Second {
		t.Errorf("DefaultDuration must be sub-second for CI use; got %v", p.DefaultDuration)
	}
	for _, v := range p.Verbs {
		if v.SustainedRPS > 50 {
			t.Errorf("smoke profile verb %q at %v RPS exceeds cap of 50", v.Verb, v.SustainedRPS)
		}
	}
}

func TestVerb_LookupMissing(t *testing.T) {
	p := reliability.NominalLoadProfile()
	if _, ok := p.Verb("agent.unknown"); ok {
		t.Error("Verb returned ok for unknown verb")
	}
}
