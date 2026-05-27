package calibration_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/calibration"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

func TestDefaultConfig_PassesValidation(t *testing.T) {
	c := calibration.DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("DefaultConfig should be valid: %v", err)
	}
	if c.ArtifactPath != calibration.DefaultArtifactPath {
		t.Errorf("ArtifactPath: want %q, got %q", calibration.DefaultArtifactPath, c.ArtifactPath)
	}
	if c.MaxInflightPerVerb <= 0 {
		t.Errorf("MaxInflightPerVerb should be > 0, got %d", c.MaxInflightPerVerb)
	}
	if c.Profile.Name != "nominal" {
		t.Errorf("Profile.Name: want %q, got %q", "nominal", c.Profile.Name)
	}
}

func TestEffectiveDuration_PrefersOverride(t *testing.T) {
	c := calibration.DefaultConfig()
	if got := c.EffectiveDuration(); got != 30*time.Minute {
		t.Errorf("default EffectiveDuration: want 30m, got %v", got)
	}
	c.Duration = 5 * time.Minute
	if got := c.EffectiveDuration(); got != 5*time.Minute {
		t.Errorf("override EffectiveDuration: want 5m, got %v", got)
	}
}

func TestConfig_Validate_Rejects_BadConfig(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*calibration.Config)
		wantSubs []string
	}{
		{
			name:     "empty ArtifactPath",
			mutate:   func(c *calibration.Config) { c.ArtifactPath = "" },
			wantSubs: []string{"ArtifactPath is required"},
		},
		{
			name:     "ArtifactPath NUL byte",
			mutate:   func(c *calibration.Config) { c.ArtifactPath = "x\x00y" },
			wantSubs: []string{"NUL byte"},
		},
		{
			name:     "ArtifactPath = .",
			mutate:   func(c *calibration.Config) { c.ArtifactPath = "./" },
			wantSubs: []string{"resolves to current directory"},
		},
		{
			name:     "zero MaxInflightPerVerb",
			mutate:   func(c *calibration.Config) { c.MaxInflightPerVerb = 0 },
			wantSubs: []string{"MaxInflightPerVerb"},
		},
		{
			name:     "negative SeededFixtureLOC",
			mutate:   func(c *calibration.Config) { c.SeededFixtureLOC = -1 },
			wantSubs: []string{"SeededFixtureLOC"},
		},
		{
			name:     "invalid profile",
			mutate:   func(c *calibration.Config) { c.Profile = reliability.LoadProfile{} },
			wantSubs: []string{"invalid profile"},
		},
		{
			name:     "duration override below 1 ms",
			mutate:   func(c *calibration.Config) { c.Duration = 500 * time.Microsecond; c.Profile.DefaultDuration = 500 * time.Microsecond },
			wantSubs: []string{"effective duration must be >= 1ms"},
		},
		{
			name: "labelled query missing query string",
			mutate: func(c *calibration.Config) {
				c.LabeledQueries = []calibration.LabeledQuery{{ExpectedNodeID: "n1"}}
			},
			wantSubs: []string{"LabeledQueries[0].Query is required"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := calibration.DefaultConfig()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatal("Validate returned nil")
			}
			for _, want := range tc.wantSubs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing substring %q", err.Error(), want)
				}
			}
		})
	}
}

// TestValidate_AcceptsPureLoadDriverLabeledQuery pins the
// contract documented at
// `docs/code-intelligence/agent-memory/load-test-calibration.md`:
// a labelled-query entry with a Query string but BOTH
// ExpectedNodeID and ExpectedConceptIDs empty is a pure
// load-driver entry that MUST pass validation. The recall
// scenario's per-sample aggregator already skips entries with
// empty expectations (see internal/loadtest/scenarios/recall.go),
// so accepting them at Validate time lets operators mix pure
// load drivers with measured queries in one fixture.
func TestValidate_AcceptsPureLoadDriverLabeledQuery(t *testing.T) {
	c := calibration.DefaultConfig()
	c.LabeledQueries = []calibration.LabeledQuery{
		{Query: "pure load driver: no learning-quality contribution"},
		{Query: "rank-only", ExpectedNodeID: "node:x"},
		{Query: "concepts-only", ExpectedConceptIDs: []string{"concept:y"}},
		{Query: "fully measured", ExpectedNodeID: "node:z", ExpectedConceptIDs: []string{"concept:w"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid mix of labelled queries: %v", err)
	}
}

func TestEnsureArtifactDir_UsesInjectedMkdir(t *testing.T) {
	c := calibration.DefaultConfig()
	c.ArtifactPath = "out/sub/report.md"
	calls := []string{}
	mkdir := func(p string, _ fs.FileMode) error {
		calls = append(calls, p)
		return nil
	}
	got, err := c.EnsureArtifactDir(mkdir)
	if err != nil {
		t.Fatalf("EnsureArtifactDir: %v", err)
	}
	wantCleaned := "out" + string([]byte{'/'}) + "sub" + string([]byte{'/'}) + "report.md"
	if got != "out/sub/report.md" && got != wantCleaned {
		// Allow OS-specific separator from filepath.Clean.
		if !strings.HasSuffix(got, "report.md") {
			t.Errorf("EnsureArtifactDir returned %q, want suffix report.md", got)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("mkdir calls: want 1, got %d", len(calls))
	}
}

func TestEnsureArtifactDir_PropagatesMkdirError(t *testing.T) {
	c := calibration.DefaultConfig()
	c.ArtifactPath = "out/report.md"
	sentinel := errors.New("disk full")
	_, err := c.EnsureArtifactDir(func(string, fs.FileMode) error { return sentinel })
	if err == nil {
		t.Fatal("EnsureArtifactDir should propagate mkdir error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain missing sentinel: %v", err)
	}
}

func TestEnsureArtifactDir_RejectsEmpty(t *testing.T) {
	c := calibration.Config{}
	_, err := c.EnsureArtifactDir(func(string, fs.FileMode) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "ArtifactPath is empty") {
		t.Errorf("EnsureArtifactDir with empty path: want error, got %v", err)
	}
}
