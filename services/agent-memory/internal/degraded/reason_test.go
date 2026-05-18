package degraded

import (
	"errors"
	"testing"
)

func TestIsClosed_canonicalReasons(t *testing.T) {
	t.Parallel()
	closed := []string{
		ReasonEpisodicLogUnavailable,
		ReasonGraphStoreUnavailable,
		ReasonEmbeddingIndexUnavailable,
		ReasonRerankerModelStale,
		ReasonSpanIngestorBackpressure,
		ReasonConsolidatorBackpressure,
	}
	for _, r := range closed {
		if !IsClosed(r) {
			t.Errorf("IsClosed(%q) = false; want true", r)
		}
	}
}

func TestIsClosed_rejectsNonClosed(t *testing.T) {
	t.Parallel()
	for _, r := range []string{
		"",
		"oops",
		"summariser_unavailable", // summarize.go's internal classifier value — NOT closed-set
		"qdrant_partition_split",
		"reranker_model_stale ", // trailing space
		"Episodic_log_unavailable",
	} {
		if IsClosed(r) {
			t.Errorf("IsClosed(%q) = true; want false", r)
		}
	}
}

func TestAllReasons_isClosedSet(t *testing.T) {
	t.Parallel()
	all := AllReasons()
	if len(all) != 6 {
		t.Fatalf("AllReasons() returned %d reasons; want 6", len(all))
	}
	seen := make(map[string]struct{}, len(all))
	for _, r := range all {
		if !IsClosed(r) {
			t.Errorf("AllReasons() returned %q which is not closed", r)
		}
		if _, dup := seen[r]; dup {
			t.Errorf("AllReasons() duplicated %q", r)
		}
		seen[r] = struct{}{}
	}
}

func TestEnforce_happyShapes(t *testing.T) {
	t.Parallel()
	if err := Enforce(false, ""); err != nil {
		t.Errorf("Enforce(false, \"\") = %v; want nil", err)
	}
	for _, r := range AllReasons() {
		if err := Enforce(true, r); err != nil {
			t.Errorf("Enforce(true, %q) = %v; want nil", r, err)
		}
	}
}

func TestEnforce_rejectsBadShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		degraded bool
		reason   string
	}{
		{"degraded with empty reason", true, ""},
		{"degraded with non-closed reason", true, "oops"},
		{"degraded with stale legacy reason", true, "qdrant_partition_split"},
		{"not-degraded with non-empty reason", false, "graph_store_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Enforce(tc.degraded, tc.reason)
			if err == nil {
				t.Fatalf("Enforce(%v, %q) = nil; want error", tc.degraded, tc.reason)
			}
			if !errors.Is(err, ErrUnknownReason) {
				t.Errorf("err not Is ErrUnknownReason: %v", err)
			}
			got, ok := Reason(err)
			if !ok {
				t.Errorf("Reason(err) = _, false; want true")
			}
			if got != tc.reason {
				t.Errorf("Reason(err) = %q; want %q", got, tc.reason)
			}
		})
	}
}

func TestPriority_orderingMatchesContract(t *testing.T) {
	t.Parallel()
	// hard outages dominate over staleness; staleness over
	// backpressure; backpressure over unknown.
	checks := []struct {
		a, b string
	}{
		{ReasonEpisodicLogUnavailable, ReasonGraphStoreUnavailable},
		{ReasonGraphStoreUnavailable, ReasonEmbeddingIndexUnavailable},
		{ReasonEmbeddingIndexUnavailable, ReasonRerankerModelStale},
		{ReasonRerankerModelStale, ReasonSpanIngestorBackpressure},
		{ReasonSpanIngestorBackpressure, ReasonConsolidatorBackpressure},
		{ReasonConsolidatorBackpressure, "oops"},
		{ReasonConsolidatorBackpressure, ""},
	}
	for _, c := range checks {
		if Priority(c.a) <= Priority(c.b) {
			t.Errorf("Priority(%q)=%d should exceed Priority(%q)=%d",
				c.a, Priority(c.a), c.b, Priority(c.b))
		}
	}
}
