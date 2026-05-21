// Package calibration owns the Stage 8.4 calibration-harness
// configuration and report-rendering surface.
//
// The package is intentionally network- and clock-free: a
// Config is plain data the harness consumes, and a Report is
// plain data the harness produces. The harness binary at
// `cmd/loadtest-harness` wires these against a real
// AgentClient + ManagementClient; tests under
// `internal/loadtest` exercise the same data shapes against
// fakes.
package calibration

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// Config is the immutable input the calibration harness reads
// at startup. Construct via [DefaultConfig] and override
// fields before passing to `loadtest.NewHarness`.
type Config struct {
	// Profile is the §8.3-derived traffic envelope the
	// harness drives. Required. The harness rejects an empty
	// profile via [Config.Validate].
	Profile reliability.LoadProfile

	// Duration overrides Profile.DefaultDuration. Zero falls
	// back to Profile.DefaultDuration (the operator pattern:
	// `loadtest-harness` with no `-duration` flag honours the
	// profile's default).
	Duration time.Duration

	// AgentTarget is the dial string for the Agent Surface
	// gRPC endpoint (mTLS) — e.g. "agent-api:8443". Empty
	// when the harness is driven against a fake AgentClient
	// (unit tests), in which case the production main()
	// rejects the config so the operator does not run the
	// harness against the prod default.
	AgentTarget string

	// ManagementTarget is the dial string for the Management
	// Surface REST endpoint — e.g. "https://mgmt-api:8444".
	// Empty when not driving mgmt.ingest_spans.
	ManagementTarget string

	// ArtifactPath is the on-disk path the harness writes the
	// calibration report markdown to. Defaults to
	// `docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md`
	// — the path the implementation plan pins.
	ArtifactPath string

	// RepoID scopes the §8.3 driven verbs to a specific
	// repository fixture. The plan's seeded 200 k LOC fixture
	// gets one repo_id; multi-repo soak runs supply a list
	// the scenarios round-robin across.
	RepoID string

	// SeededFixtureLOC is the line-count of the seeded
	// fixture the operator drove the harness against. Stamped
	// onto the artifact so reviewers can spot a calibration
	// run on the wrong-size repo.
	SeededFixtureLOC int

	// RandomSeed makes a calibration run reproducible: the
	// scenario rng is seeded from this and the
	// labelled-query selection / synthetic payload generation
	// becomes deterministic. Zero means "use a wall-clock
	// seed", which the harness logs so an operator can replay
	// a flaky run by passing the logged seed back.
	RandomSeed int64

	// MaxInflightPerVerb is the per-verb open-loop concurrency
	// cap. The scheduler fires planned arrivals into a bounded
	// semaphore; arrivals that would exceed the cap are
	// recorded as "dropped" in the verb result (so the
	// harness reports both planned and achieved RPS instead
	// of silently slipping into closed-loop mode).
	//
	// Default 256 — derived from the §8.3 envelope's worst
	// case: agent.recall = 50 RPS × 4 s p99 = 200 concurrent
	// requests when the service is exactly on its SLO line.
	// 256 leaves ~28 % headroom so a service meeting its
	// envelope does NOT spuriously trip drop-tick accounting,
	// while still bounding worst-case memory.
	MaxInflightPerVerb int

	// LabeledQueries is the fixture set the harness uses to
	// measure the learning-quality SLOs (rank-of-correct-node
	// and concept-hit fraction at K). Empty disables the
	// learning-quality measurement; the harness reports
	// `n/a` in the artifact and the operator sees the gap.
	LabeledQueries []LabeledQuery

	// SyntheticContextIDs pre-seeds the agent.observe
	// scenario with a pool of context_ids so observe latency
	// is NOT correlated with recall latency. The harness
	// round-robins through the pool; an empty pool falls
	// back to generating fresh UUIDs each tick (still
	// independent of recall, but the wire-level
	// context_id-not-found branch is exercised more
	// heavily — flag in the artifact when this fallback
	// kicked in).
	SyntheticContextIDs []string

	// Provenance is a short operator-supplied tag that the
	// report renderer surfaces as a prominent banner at the
	// top of the artifact. It exists so a reviewer of the
	// persisted markdown can immediately distinguish an
	// in-process stub baseline (gen_artifact.go) from a real
	// deploy/local-stack §8.3 calibration WITHOUT cross-
	// referencing the operator doc.
	//
	// Suggested values:
	//   - "IN-PROCESS STUB BASELINE — gen_artifact.go against
	//     httptest + grpc stubs; NOT a §8.3 production seal."
	//   - "DEPLOY/LOCAL STACK NOMINAL CALIBRATION — seeded
	//     200 k LOC fixture; §8.3 production seal."
	//
	// Empty means "no banner"; the artifact renders without
	// the provenance section but the generator-stamp paragraph
	// still appears.
	Provenance string
}

// LabeledQuery is one fixture entry the harness drives the
// agent.recall scenario with to measure learning-quality. The
// expected_node_id / expected_concept_ids carry the ground
// truth the harness compares against the recall response.
//
// An entry with a non-empty Query but BOTH expected fields
// empty is accepted as a pure load-driver entry: the harness
// still issues the recall, exercising the hot path, but the
// entry contributes nothing to rank / concept-hit aggregation
// (the recall scenario skips empty-expectation entries when
// folding learning-quality). This matches the operator doc at
// `docs/code-intelligence/agent-memory/load-test-calibration.md`
// which advertises both-empty as opt-out for that query.
//
// IMPORTANT — proxy semantics (also documented in
// reliability.LearningQualityTargets): the harness measures
// labelled-query ranks against the recall response payload, NOT
// the §8.3 post-hoc Episode/Observation/RecallContextLog join.
// The values reported in the artifact are explicitly tagged as
// "labelled-query proxy" so the operator does not mistake them
// for the contract-level SLO measurement.
type LabeledQuery struct {
	// Query is the natural-language string the harness sends
	// as `RecallRequest.query`.
	Query string

	// ExpectedNodeID is the node id the harness expects to
	// find ranked highly in `RecallResponse.nodes`. Empty
	// disables rank measurement for this query.
	ExpectedNodeID string

	// ExpectedConceptIDs is the set of concept ids the
	// harness expects to see in `RecallResponse.concepts` at
	// least one of. Empty disables concept-hit measurement
	// for this query.
	ExpectedConceptIDs []string

	// Kinds optionally narrows the `RecallRequest.kinds`
	// filter. Empty defaults to the recall scenario's
	// default kind set.
	Kinds []string
}

// DefaultConfig returns a Config seeded from the §8.3
// envelope. Operators override fields (Duration, AgentTarget,
// ArtifactPath, LabeledQueries, RandomSeed) via flags on the
// `loadtest-harness` binary; tests construct one directly.
func DefaultConfig() Config {
	return Config{
		Profile:            reliability.NominalLoadProfile(),
		ArtifactPath:       DefaultArtifactPath,
		MaxInflightPerVerb: 256,
	}
}

// DefaultArtifactPath is where the calibration harness writes
// the report. The implementation plan pins this path
// (`docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md`)
// — the doc lives in the story tree, not under
// `docs/code-intelligence/agent-memory/` which the C#-style
// brief template guessed.
const DefaultArtifactPath = "docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md"

// EffectiveDuration returns the operator-overridden Duration
// or, when zero, the profile's default.
func (c Config) EffectiveDuration() time.Duration {
	if c.Duration > 0 {
		return c.Duration
	}
	return c.Profile.DefaultDuration
}

// Validate returns a non-nil error when the harness should
// refuse to start (no profile, sub-millisecond effective
// duration, etc). The error string aggregates every failure
// via errors.Join so the operator fixes them in one edit.
func (c Config) Validate() error {
	var errs []error
	if err := c.Profile.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("calibration: invalid profile: %w", err))
	}
	if c.EffectiveDuration() < time.Millisecond {
		errs = append(errs, fmt.Errorf("calibration: effective duration must be >= 1ms (got %v)", c.EffectiveDuration()))
	}
	if c.ArtifactPath == "" {
		errs = append(errs, errors.New("calibration: ArtifactPath is required"))
	} else {
		// Reject paths with NUL bytes early; clean up the
		// path so the report writer doesn't double-traverse
		// `..` segments.
		if strings.ContainsRune(c.ArtifactPath, 0) {
			errs = append(errs, errors.New("calibration: ArtifactPath contains NUL byte"))
		}
		if filepath.Clean(c.ArtifactPath) == "." {
			errs = append(errs, fmt.Errorf("calibration: ArtifactPath %q resolves to current directory", c.ArtifactPath))
		}
	}
	if c.MaxInflightPerVerb <= 0 {
		errs = append(errs, fmt.Errorf("calibration: MaxInflightPerVerb must be > 0 (got %d)", c.MaxInflightPerVerb))
	}
	if c.SeededFixtureLOC < 0 {
		errs = append(errs, fmt.Errorf("calibration: SeededFixtureLOC must be >= 0 (got %d)", c.SeededFixtureLOC))
	}
	for i, lq := range c.LabeledQueries {
		if lq.Query == "" {
			errs = append(errs, fmt.Errorf("calibration: LabeledQueries[%d].Query is required", i))
		}
		// NOTE: an entry with empty ExpectedNodeID AND empty
		// ExpectedConceptIDs is intentionally accepted — it
		// becomes a "pure load driver" query (exercises the
		// recall hot path, contributes nothing to
		// learning-quality aggregation). The operator doc
		// (`docs/code-intelligence/agent-memory/load-test-calibration.md`)
		// documents this contract; the rank / concept-hit
		// aggregator in `internal/loadtest/scenarios/recall.go`
		// already skips entries with empty expectations.
	}
	return errors.Join(errs...)
}

// EnsureArtifactDir creates the parent directory of
// ArtifactPath, returning the cleaned absolute-style path the
// report writer should use. mkdirAll is the injected mkdir
// (production wires os.MkdirAll; tests wire an in-memory
// fake or a temp-dir helper).
func (c Config) EnsureArtifactDir(mkdirAll func(string, fs.FileMode) error) (string, error) {
	if c.ArtifactPath == "" {
		return "", errors.New("calibration: ArtifactPath is empty")
	}
	cleaned := filepath.Clean(c.ArtifactPath)
	dir := filepath.Dir(cleaned)
	if dir != "." && dir != "/" && dir != "" {
		if err := mkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("calibration: mkdir %q: %w", dir, err)
		}
	}
	return cleaned, nil
}
