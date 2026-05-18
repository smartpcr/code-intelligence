package degraded

import (
	"sync"
	"testing"
)

func TestMapFaultInjector_perVerb(t *testing.T) {
	t.Parallel()
	inj := NewMapFaultInjector()
	inj.SetForVerb("agent.recall", "oops")

	deg, reason := inj.Inject("agent.recall", "repo-1")
	if !deg || reason != "oops" {
		t.Errorf("Inject(recall, repo-1) = (%v, %q); want (true, oops)", deg, reason)
	}

	// Different verb is unaffected.
	if deg, _ := inj.Inject("agent.observe", "repo-1"); deg {
		t.Errorf("observe should not be flipped")
	}
}

func TestMapFaultInjector_perRepoOverridesWildcard(t *testing.T) {
	t.Parallel()
	inj := NewMapFaultInjector()
	inj.SetForVerb("agent.recall", "wild")
	inj.SetForVerbRepo("agent.recall", "repo-1", ReasonGraphStoreUnavailable)

	deg, reason := inj.Inject("agent.recall", "repo-1")
	if !deg || reason != ReasonGraphStoreUnavailable {
		t.Errorf("Inject(recall, repo-1) = (%v, %q); want (true, graph_store_unavailable)",
			deg, reason)
	}

	deg, reason = inj.Inject("agent.recall", "repo-2")
	if !deg || reason != "wild" {
		t.Errorf("Inject(recall, repo-2) wildcard = (%v, %q); want (true, wild)",
			deg, reason)
	}
}

func TestMapFaultInjector_emptyReasonNoInject(t *testing.T) {
	t.Parallel()
	inj := NewMapFaultInjector()
	inj.SetForVerb("agent.recall", "")
	if deg, _ := inj.Inject("agent.recall", "repo-1"); deg {
		t.Errorf("empty reason should not trigger injection")
	}
}

func TestMapFaultInjector_clearAndClearVerb(t *testing.T) {
	t.Parallel()
	inj := NewMapFaultInjector()
	inj.SetForVerb("agent.recall", "oops")
	inj.SetForVerb("agent.observe", "oops")

	inj.ClearVerb("agent.recall")
	if deg, _ := inj.Inject("agent.recall", "any"); deg {
		t.Errorf("ClearVerb did not clear recall")
	}
	if deg, _ := inj.Inject("agent.observe", "any"); !deg {
		t.Errorf("ClearVerb cleared too much")
	}

	inj.Clear()
	if deg, _ := inj.Inject("agent.observe", "any"); deg {
		t.Errorf("Clear did not clear observe")
	}
}

func TestMapFaultInjector_concurrent(t *testing.T) {
	t.Parallel()
	inj := NewMapFaultInjector()
	inj.SetForVerb("agent.observe", ReasonConsolidatorBackpressure)

	const readers = 32
	const reads = 50
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < reads; j++ {
				deg, reason := inj.Inject("agent.observe", "repo-1")
				if !deg || reason != ReasonConsolidatorBackpressure {
					t.Errorf("Inject = (%v, %q); want backpressure", deg, reason)
				}
			}
		}()
	}
	wg.Wait()
}

func TestFaultInjectorFunc(t *testing.T) {
	t.Parallel()
	var inj FaultInjector = FaultInjectorFunc(func(verb, repoID string) (bool, string) {
		if verb == "agent.recall" {
			return true, ReasonGraphStoreUnavailable
		}
		return false, ""
	})
	deg, reason := inj.Inject("agent.recall", "repo")
	if !deg || reason != ReasonGraphStoreUnavailable {
		t.Errorf("FaultInjectorFunc misbehaved: (%v, %q)", deg, reason)
	}
	if deg, _ := inj.Inject("agent.observe", "repo"); deg {
		t.Errorf("FaultInjectorFunc returned true for unset verb")
	}
}
