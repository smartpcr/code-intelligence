package aggregator

import (
	"context"
	"sync"

	"github.com/gofrs/uuid"
)

// SystemTierInputSource is the read-side dependency the
// Cross-Repo Aggregator pulls per-tick system-tier inputs
// from. Each call returns ONE [SystemTierInput] per
// `(repo_id, sha)` pair the composer should produce
// system-tier rows for.
//
// # Contract
//
//   - One call per [Aggregator.Tick]. The source MUST capture
//     a consistent point-in-time read across all returned
//     inputs (e.g. via a snapshot-isolation txn or a
//     READ-COMMITTED query whose result the source freezes
//     in-memory) so the composer does not see torn state
//     across rows.
//   - The set of returned inputs MAY be empty -- a fresh
//     deployment with no `metric_sample_active` rows yields
//     zero inputs and the aggregator writes zero system-tier
//     rows that tick.
//   - Each input's `Mode` is the deployment mode the source
//     was constructed against. In embedded mode (default
//     v1), `XRepoEdges` / `CallEdges` are empty AND
//     `XRepoEdgesAvailable` / `CallEdgesAvailable` are
//     false; the composer's fail-safe contract degrades
//     the cross-repo-edge-dependent kinds with
//     `xrepo_edges_unavailable`. In linked mode (Stage 8.7
//     opt-in), the source MUST set the availability flags
//     true on every input it returns -- the composer's
//     "available" semantics rely on the source being the
//     canonical reporter.
//   - The returned inputs MUST be deterministic for a given
//     point-in-time read (G6): two calls with the same DB
//     state produce identical output. The composer's
//     determinism contract relies on input determinism.
//   - The source MUST filter SYSTEM-tier rows out of its
//     foundation-sample input set (`pack='system'` rows
//     are NOT foundation inputs to the system-tier
//     composer; feeding them back would create a
//     definitional cycle). Foundation rows are
//     `pack IN ('base', 'solid', 'ingested')`.
//
// # Concurrency
//
// Implementations MUST be safe for concurrent invocation;
// the aggregator currently calls the source once per tick
// (no concurrency observed), but tests drive concurrent
// ticks against the same source to exercise that property.
type SystemTierInputSource interface {
	ReadSystemTierInputs(ctx context.Context) ([]SystemTierInput, error)
}

// InMemorySystemTierInputSource is the test-side
// [SystemTierInputSource]. Constructed with a fixed slice of
// inputs; every ReadSystemTierInputs call returns a deep
// copy.
//
// Concurrency: read-only after construction; safe for use
// across goroutines.
type InMemorySystemTierInputSource struct {
	inputs []SystemTierInput
	// failErr, when non-nil, is returned by every
	// ReadSystemTierInputs call without yielding inputs.
	// Tests set it to simulate a PG outage.
	mu      sync.Mutex
	failErr error
}

// NewInMemorySystemTierInputSource COPIES the input slice (and
// each input's nested slices/maps) so subsequent mutations to
// the caller's locals do not perturb the source.
func NewInMemorySystemTierInputSource(inputs []SystemTierInput) *InMemorySystemTierInputSource {
	cp := make([]SystemTierInput, len(inputs))
	for i, in := range inputs {
		cp[i] = deepCopySystemTierInput(in)
	}
	return &InMemorySystemTierInputSource{inputs: cp}
}

// SetFailError configures the source to return `err` on every
// subsequent ReadSystemTierInputs call. Pass nil to clear.
func (s *InMemorySystemTierInputSource) SetFailError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failErr = err
}

// ReadSystemTierInputs implements [SystemTierInputSource].
// Returns a fresh deep-copy slice on each call so the caller
// can safely mutate its locals.
func (s *InMemorySystemTierInputSource) ReadSystemTierInputs(ctx context.Context) ([]SystemTierInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr != nil {
		return nil, s.failErr
	}
	out := make([]SystemTierInput, len(s.inputs))
	for i, in := range s.inputs {
		out[i] = deepCopySystemTierInput(in)
	}
	return out, nil
}

// deepCopySystemTierInput returns a value-semantics copy of
// `in` that shares no pointer or slice/map backing memory with
// the original. Used by the in-memory source's COPY-IN +
// COPY-OUT semantics so tests can mutate locals without
// surprising the captured input set.
func deepCopySystemTierInput(in SystemTierInput) SystemTierInput {
	out := SystemTierInput{
		Mode:                in.Mode,
		RepoID:              in.RepoID,
		SHA:                 in.SHA,
		ProducerRunID:       in.ProducerRunID,
		XRepoEdgesAvailable: in.XRepoEdgesAvailable,
		CallEdgesAvailable:  in.CallEdgesAvailable,
	}
	if in.Scopes != nil {
		out.Scopes = append([]ScopeRef(nil), in.Scopes...)
	}
	if in.Foundation != nil {
		out.Foundation = make([]FoundationSample, len(in.Foundation))
		for i, fs := range in.Foundation {
			out.Foundation[i] = FoundationSample{
				ScopeID:    fs.ScopeID,
				ScopeKind:  fs.ScopeKind,
				MetricKind: fs.MetricKind,
				Value:      fs.Value,
				Attrs:      copyAttrs(fs.Attrs),
			}
		}
	}
	if in.XRepoEdges != nil {
		out.XRepoEdges = append([]XRepoEdge(nil), in.XRepoEdges...)
	}
	if in.CallEdges != nil {
		out.CallEdges = append([]CallEdge(nil), in.CallEdges...)
	}
	if in.VelocityWindows != nil {
		out.VelocityWindows = append([]float64(nil), in.VelocityWindows...)
	}
	if in.AuthorsByScope != nil {
		out.AuthorsByScope = make(map[uuid.UUID][]string, len(in.AuthorsByScope))
		for k, v := range in.AuthorsByScope {
			out.AuthorsByScope[k] = append([]string(nil), v...)
		}
	}
	return out
}
