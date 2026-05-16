// Package spaningestor implements the §4 Span Ingestor pipeline
// for the agent-memory service.  This file owns Stage 4.1's OTel
// attribute resolver: a pure function over the structural graph
// that maps an incoming OTel span onto a Method (and optionally
// a Block) Node per `tech-spec.md` §8.6.
//
// Why a dedicated package
// -----------------------
// The Span Ingestor worker (Stage 4.2, `cmd/span-ingestor/main.go`,
// not yet built) consumes OTLP span batches, hands each span to
// this resolver, and — on a hit — calls `graphwriter`'s
// `InsertObservedCallsEdge` / `TraceObservation*` paths to persist
// the observation.  Splitting the resolver out of that worker
// keeps the §8.6 attribute-mapping logic testable without standing
// up an OTLP receiver or a Postgres pool, and keeps the
// observability of "what fraction of spans land on a known
// Method" (the `span_unresolved_total` counter) localized to one
// place.
//
// Why a `Lookup` interface rather than a direct `*graphreader.Reader`
// -----------------------------------------------------------------
// The resolver's needs are narrower than the full reader surface
// (it only needs three queries; it never enumerates edges, never
// walks contains-trees, never reads embedding payloads).  A small
// interface keeps the unit tests fast (in-memory fakes only) and
// the Stage 4.2 worker free to swap in a caching layer in front
// of the reader without changing the resolver's surface.
//
// What this file does NOT do
// --------------------------
//   - It does NOT consume OTLP gRPC frames — Stage 4.2 owns the
//     receiver and normalizes OTLP attributes (which arrive
//     union-typed: string / int / bool / double) into the
//     `map[string]string` shape this resolver consumes.
//   - It does NOT mutate the graph — `InsertObservedCallsEdge`
//     is a writer concern.  The resolver's output is a typed
//     `Resolution` value the worker decides how to persist.
//   - It does NOT recursively resolve `parent_span_id` to the
//     caller side of an `observed_calls` Edge — that walk is the
//     worker's concern (§8.6 row 3), implemented in Stage 4.2.
package spaningestor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	astpkg "github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// Attribute names per OTel semantic conventions §"Source code
// attributes" (the v1.27 names; `code.signature` is preserved
// from the v1.20 schema for languages that still emit it as the
// overload disambiguator).  Centralized here so the resolver's
// only string literals for attribute keys live in one place — a
// schema rev only needs an edit here, not throughout the
// resolution ladder.
const (
	AttrCodeNamespace = "code.namespace"
	AttrCodeFunction  = "code.function"
	AttrCodeSignature = "code.signature"
	AttrCodeFilepath  = "code.filepath"
	AttrCodeLineno    = "code.lineno"
)

// Span is the minimal OTel span view the resolver consumes.  The
// Span Ingestor worker (Stage 4.2) builds these from incoming
// OTLP frames; the resolver's contract is intentionally agnostic
// to the wire format so the worker can normalize OTLP's
// union-typed attribute values (string / int / bool / double)
// into a single `map[string]string` shape here.
//
// `RepoID` is resolved upstream from `service.name` /
// `service.instance.id` — the resolver itself never queries the
// repo registry; a zero `RepoID` will simply produce zero
// candidates from the lookup (and is exercised by an explicit
// test so the behaviour does not regress).
type Span struct {
	RepoID       string
	TraceID      string
	SpanID       string
	ParentSpanID string
	Attributes   map[string]string
}

// MethodCandidate is the read-shape of one Method Node returned
// by the Lookup interface.  Mirrors the fields the resolver
// actually needs from `graphreader.Node` — we deliberately do
// NOT re-export the full Node type so the test-side fakes stay
// minimal and the resolver does not couple to the reader's
// retirement / fingerprint surface.
//
// `ParamSignature` is the normalized parameter list extracted by
// the Lookup implementation from the Method's
// `canonical_signature` (which uses the form
// `<repoURL>::method::<relPath>#<qualifiedName>(<params>)` per
// `internal/repoindexer/ast/dispatcher.go.methodSignature`).
// The resolver consults `ParamSignature` ONLY to filter overload
// ambiguity against `code.signature` — see chooseMethod.
type MethodCandidate struct {
	NodeID             string
	CanonicalSignature string
	FilePath           string // relative path, forward-slash form
	ParamSignature     string
	BodyStartLine      int // 1-based, inclusive
	BodyEndLine        int // 1-based, inclusive
}

// BlockCandidate is the read-shape of one Block Node.  The
// resolver consults `StartLine` / `EndLine` (file-relative,
// 1-based, inclusive — see
// `internal/repoindexer/ast/block.go.Block`) only via the
// `LookupBlockForMethod` contract.  Exposed here as a value so
// callers can render the Block on the span observation.
type BlockCandidate struct {
	NodeID             string
	CanonicalSignature string
	Kind               string // entry / branch / loop_body / exception / exit (closed set per ast.BlockKind)
	StartLine          int    // 1-based file-relative
	EndLine            int    // 1-based file-relative, inclusive
}

// Lookup is the narrow read-side abstraction the resolver depends
// on.  Stage 4.2 wires a graphreader-backed implementation; the
// unit tests in this package pass an in-memory fake.
//
// Contract notes that the production binding (Stage 4.2) must
// honour and the test fakes already do:
//
//   - `LookupMethodsByName` matches `namespace` LITERALLY — an
//     empty `namespace` matches only Methods whose canonical
//     namespace is genuinely empty (e.g. free functions in
//     procedural languages), NOT a global search.  This protects
//     against false positives when an OTel emitter forgets to
//     set `code.namespace`.  Note: the v1 resolver per tech-spec
//     §8.6 will not call `LookupMethodsByName` unless BOTH
//     `code.namespace` AND `code.function` are non-empty (the
//     mapping table says "if either is missing, fall back to
//     filepath + lineno"), so production callers may treat an
//     empty `namespace` as a wiring bug; the literal-match
//     contract is retained for defence-in-depth and to keep the
//     interface usable from ad-hoc tools.
//
//   - `LookupMethodByLocation` receives `filepath` in
//     forward-slash, repo-relative form.  The resolver
//     normalizes (strips `./` prefix, converts backslashes) before
//     calling, so the production binding can hash directly into
//     its filepath index.
//
//   - `LookupBlockForMethod` returns the MOST SPECIFIC Block
//     whose [StartLine, EndLine] interval (inclusive on both
//     ends) covers `lineno`.  When no Block matches, returns
//     `(nil, nil)` — never an error.  An error return is
//     reserved for backend failures (network, SQL).
//
// All three methods MUST return `nil, nil` (no error, no
// candidate) when the lookup runs successfully but finds
// nothing — this is the "miss" signal the resolver uses to
// step down the §8.6 ladder.  Returning an error short-circuits
// the entire span (Resolve returns the error to its caller and
// does NOT increment `span_unresolved_total` — a backend outage
// is not a span-quality signal).
type Lookup interface {
	LookupMethodsByName(
		ctx context.Context, repoID, namespace, function string,
	) ([]MethodCandidate, error)

	LookupMethodByLocation(
		ctx context.Context, repoID, filepath string, lineno int,
	) (*MethodCandidate, error)

	LookupBlockForMethod(
		ctx context.Context, methodNodeID string, lineno int,
	) (*BlockCandidate, error)
}

// ResolutionStatus discriminates the resolver's three terminal
// outcomes.  `StatusUnresolved` is the zero value so a default-
// constructed `Resolution{}` is unambiguously the drop case.
type ResolutionStatus int

const (
	// StatusUnresolved means the §8.6 ladder bottomed out without
	// finding a Method.  The Span Ingestor worker MUST NOT
	// persist an `observed_calls` Edge or a `TraceObservationLog`
	// row in this state (the architecture-pinned "no synthetic
	// Node" invariant from §8.6).  The `span_unresolved_total`
	// counter has already been incremented by the resolver.
	StatusUnresolved ResolutionStatus = iota
	// StatusMethod means the resolver mapped the span to a Method
	// Node but not to a specific Block — either `code.lineno` was
	// absent / unparseable, or no Block covers the supplied line.
	// Per §8.6 the worker attaches the observation to the Method
	// (the "fallback to parent Method" branch on the Block-lookup
	// miss row of the mapping table).
	StatusMethod
	// StatusBlock means the resolver mapped the span to BOTH a
	// Method AND a specific Block within that Method.  The worker
	// uses the Block as the observation anchor.
	StatusBlock
)

// String renders the status for log lines / test failures.
func (s ResolutionStatus) String() string {
	switch s {
	case StatusUnresolved:
		return "unresolved"
	case StatusMethod:
		return "method"
	case StatusBlock:
		return "block"
	default:
		return fmt.Sprintf("ResolutionStatus(%d)", int(s))
	}
}

// ResolutionReason classifies WHY a given `Resolution` ended up
// at its `Status`.  This is the METHOD-side classification only
// — how the resolver decided whether a Method was found and via
// which rung.  Block-attachment detail lives in `BlockOutcome`
// so the two dimensions stay orthogonal (a Method matched via
// name with no Block attached is `{Reason: NameMatched, Block:
// NoLineno}`, not a fused enum value that explodes
// combinatorially).
//
// Intended as a diagnostic signal for structured logs and
// tests; downstream worker code MUST NOT branch on the Reason
// string form (use `Status` for control flow).  The rubber-duck
// pass on this file flagged "stringly-typed reasons can become
// accidental API" — the enum form here is the structural answer
// to that concern.
type ResolutionReason int

const (
	// ReasonUnset is the zero value; never produced by Resolve.
	ReasonUnset ResolutionReason = iota
	// ReasonNameMatched: the namespace+function lookup yielded a
	// single Method (or disambiguated via `code.signature`).
	// Status will be StatusMethod or StatusBlock — see
	// `BlockOutcome` for which.
	ReasonNameMatched
	// ReasonLocationMatched: the filepath+lineno fallback located
	// the enclosing Method.
	ReasonLocationMatched
	// ReasonNoNameMatch: `code.function` was supplied but the
	// name-lookup returned zero candidates.  Surfaces on
	// Status=Unresolved when no filepath fallback rescued.
	ReasonNoNameMatch
	// ReasonAmbiguousName: name-lookup returned multiple
	// candidates and `code.signature` was empty (or did not
	// disambiguate).
	ReasonAmbiguousName
	// ReasonSignatureMismatch: name-lookup returned candidate(s)
	// but `code.signature` did not match any candidate's
	// `ParamSignature`.  Accepting a contradictory unique
	// candidate would pollute observation aggregates (rubber-duck
	// blocker #2), so we step down to the filepath rung instead.
	ReasonSignatureMismatch
	// ReasonMissingAllAttributes: neither `code.function` nor
	// `code.filepath` was set — the ladder had nothing to chew on.
	ReasonMissingAllAttributes
	// ReasonNoFilepathMatch: filepath fallback ran but found no
	// Method covering the supplied line.
	ReasonNoFilepathMatch
)

// String renders the reason for log lines.
func (r ResolutionReason) String() string {
	switch r {
	case ReasonUnset:
		return "unset"
	case ReasonNameMatched:
		return "name_matched"
	case ReasonLocationMatched:
		return "location_matched"
	case ReasonNoNameMatch:
		return "no_name_match"
	case ReasonAmbiguousName:
		return "ambiguous_name"
	case ReasonSignatureMismatch:
		return "signature_mismatch"
	case ReasonMissingAllAttributes:
		return "missing_all_attributes"
	case ReasonNoFilepathMatch:
		return "no_filepath_match"
	default:
		return fmt.Sprintf("ResolutionReason(%d)", int(r))
	}
}

// BlockOutcome classifies the Block-attachment side of a
// Resolution.  Set only when a Method was found (Status =
// StatusMethod or StatusBlock); zero (`BlockOutcomeNotAttempted`)
// when no Method was resolved.  Kept separate from
// `ResolutionReason` so the two outcome dimensions
// (method-side, block-side) do not need a Cartesian-product
// enum.
type BlockOutcome int

const (
	// BlockOutcomeNotAttempted: no Method was resolved, so block
	// lookup was never attempted.  The zero value.
	BlockOutcomeNotAttempted BlockOutcome = iota
	// BlockOutcomeMatched: a Block under the Method covered the
	// `code.lineno`.  Status will be StatusBlock.
	BlockOutcomeMatched
	// BlockOutcomeNoLineno: `code.lineno` was absent so the
	// resolver skipped block lookup.  Status = StatusMethod.
	BlockOutcomeNoLineno
	// BlockOutcomeLinenoUnparseable: `code.lineno` was present but
	// did not parse as a positive integer; block lookup skipped.
	// Status = StatusMethod.
	BlockOutcomeLinenoUnparseable
	// BlockOutcomeOutsideBlock: `code.lineno` parsed and block
	// lookup ran, but no Block under the Method covered the line.
	// Per §8.6 the observation attaches to the Method.  Status =
	// StatusMethod.
	BlockOutcomeOutsideBlock
	// BlockOutcomeLookupFailed: the Lookup backend returned an
	// error during block lookup.  Per §8.6 the Method
	// observation is still recorded (the block lookup failure
	// does not invalidate the already-resolved Method); the
	// failure is logged for operator visibility.  Status =
	// StatusMethod.
	BlockOutcomeLookupFailed
)

// String renders the block outcome for log lines.
func (o BlockOutcome) String() string {
	switch o {
	case BlockOutcomeNotAttempted:
		return "not_attempted"
	case BlockOutcomeMatched:
		return "matched"
	case BlockOutcomeNoLineno:
		return "no_lineno"
	case BlockOutcomeLinenoUnparseable:
		return "lineno_unparseable"
	case BlockOutcomeOutsideBlock:
		return "outside_block"
	case BlockOutcomeLookupFailed:
		return "lookup_failed"
	default:
		return fmt.Sprintf("BlockOutcome(%d)", int(o))
	}
}

// Resolution is the resolver's per-span output.  The worker
// switches on `Status` to decide whether to persist the
// observation; `Method` and `Block` carry the destination Nodes
// (populated according to `Status`); `Reason` carries the
// method-side diagnostic classification and `BlockOutcome` the
// block-side.
type Resolution struct {
	Status       ResolutionStatus
	Method       *MethodCandidate
	Block        *BlockCandidate
	Reason       ResolutionReason
	BlockOutcome BlockOutcome
}

// Metrics carries the per-repo counters the resolver writes.
// Mirrors the Prometheus `span_unresolved_total{repo_id=...}`
// counter the operator dashboard scrapes (per `tech-spec.md`
// §8.6 / risk §9.11 — the calibration signal that tells us when
// the OTel mapping needs revising).
//
// Construct via NewMetrics; the zero value is unusable (the
// underlying map is not initialised by the type itself so the
// allocator chooses a sensible bucket count).
//
// Concurrency: every counter increment takes a brief mutex and
// then performs an atomic add — the mutex is held only during
// the lazy first-touch insert of a per-repo `*atomic.Int64`.
// Subsequent reads on the same repo go through the map without
// blocking writers (mutex contention is acquire-only after the
// initial allocation).  Reads via Snapshot acquire the mutex
// for the full iteration so the snapshot is consistent.
type Metrics struct {
	mu sync.RWMutex
	// unresolved maps `repo_id` → counter.  Per-repo
	// allocation is lazy: a repo that never produces an
	// unresolved span never appears in the map (and Snapshot
	// will therefore omit it; UnresolvedFor returns 0).
	unresolved map[string]*atomic.Int64
}

// NewMetrics constructs a ready-to-use Metrics struct.
func NewMetrics() *Metrics {
	return &Metrics{unresolved: make(map[string]*atomic.Int64)}
}

// IncUnresolved adds 1 to the per-repo unresolved counter for
// `repoID`.  Safe for concurrent use.  An empty `repoID` is
// recorded under the empty-string key so test fixtures that
// pass zero RepoID values still surface in Snapshot — the
// production wiring (Stage 4.2) is expected to validate RepoID
// upstream.
func (m *Metrics) IncUnresolved(repoID string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	c, ok := m.unresolved[repoID]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		// Re-check under the write lock: another goroutine may
		// have created the counter between the RUnlock above
		// and the Lock here.  Without this re-check we would
		// overwrite the sibling goroutine's counter and lose
		// every increment it had recorded.
		c, ok = m.unresolved[repoID]
		if !ok {
			c = new(atomic.Int64)
			m.unresolved[repoID] = c
		}
		m.mu.Unlock()
	}
	c.Add(1)
}

// UnresolvedFor reads the current per-repo unresolved count.
// Returns 0 for a repo that has never been incremented.
func (m *Metrics) UnresolvedFor(repoID string) int64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.unresolved[repoID]
	if !ok {
		return 0
	}
	return c.Load()
}

// Snapshot returns a copy of every per-repo unresolved counter
// at a single instant.  Useful for tests that want to assert
// the total or to dump for diagnostics.  The returned map is
// caller-owned (safe to mutate).
func (m *Metrics) Snapshot() map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int64, len(m.unresolved))
	for k, v := range m.unresolved {
		out[k] = v.Load()
	}
	return out
}

// Resolver implements the §8.6 attribute-mapping ladder.
// Construct via New; Resolve is the only exported method.
//
// Resolver is safe for concurrent use: it holds no per-call
// state (the Metrics counter is mutex-protected).  The Lookup
// implementation MUST also be safe for concurrent use — every
// production binding (graphreader / pgxpool) is, the test fakes
// in this package are.
type Resolver struct {
	lookup  Lookup
	metrics *Metrics
	logger  *slog.Logger
}

// New constructs a Resolver.  A nil `lookup` panics — there is
// no useful default and a silent no-op would treat every span as
// unresolved (a misleading calibration signal).  A nil `metrics`
// is tolerated (the resolver runs without incrementing counters,
// which is the desired shape for ad-hoc tools); a nil `logger` is
// replaced with `slog.Default()`.
func New(lookup Lookup, metrics *Metrics, logger *slog.Logger) *Resolver {
	if lookup == nil {
		panic("spaningestor: New: nil Lookup")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{lookup: lookup, metrics: metrics, logger: logger}
}

// ErrLookupFailure wraps any error the Lookup implementation
// returns so callers can pattern-match `errors.Is(err,
// spaningestor.ErrLookupFailure)` to distinguish backend
// outages from "span had no matching Method".  Backend failures
// MUST NOT trip the `span_unresolved_total` counter — that is a
// span-quality signal, not an infrastructure signal (rubber-duck
// finding #6).
var ErrLookupFailure = errors.New("spaningestor: lookup backend failure")

// Resolve runs the §8.6 attribute-mapping ladder against `span`
// and returns the resolution outcome.  See the package doc for
// the contract overview; the per-rung policy is encoded here as
// straight-line control flow so a reader of this function alone
// can audit the §8.6 mapping table.
func (r *Resolver) Resolve(ctx context.Context, span Span) (Resolution, error) {
	ns := strings.TrimSpace(span.Attributes[AttrCodeNamespace])
	fn := strings.TrimSpace(span.Attributes[AttrCodeFunction])
	sig := strings.TrimSpace(span.Attributes[AttrCodeSignature])
	fp := normalizeFilepath(span.Attributes[AttrCodeFilepath])

	// Track WHY we fell through to the filepath rung so the
	// final unresolved Reason classifies the deepest miss.
	nameMissReason := ReasonMissingAllAttributes

	// ----- Rung 1: code.namespace + code.function ---------
	//
	// Per tech-spec §8.6 the name rung requires BOTH attributes:
	// "Resolve to a Method Node | code.namespace + code.function
	// | If either is missing, fall back to code.filepath +
	// code.lineno...".  We deliberately do NOT call
	// LookupMethodsByName with an empty namespace — that would
	// silently broaden the search and could surface a Method
	// from an unrelated package as a false positive.  Treating
	// "either missing" as "skip rung" makes the resolver's
	// behaviour identical to the pinned mapping table.
	if fn != "" && ns != "" {
		cands, err := r.lookup.LookupMethodsByName(ctx, span.RepoID, ns, fn)
		if err != nil {
			return Resolution{}, fmt.Errorf("%w: by name: %v", ErrLookupFailure, err)
		}
		pick, reason := chooseMethod(cands, sig)
		if pick != nil {
			return r.attachBlock(ctx, span, pick, ReasonNameMatched), nil
		}
		nameMissReason = reason
	}

	// ----- Rung 2: code.filepath + code.lineno ------------
	if fp != "" {
		lineno, ok := parseLineno(span.Attributes[AttrCodeLineno])
		if ok {
			cand, err := r.lookup.LookupMethodByLocation(ctx, span.RepoID, fp, lineno)
			if err != nil {
				return Resolution{}, fmt.Errorf("%w: by location: %v", ErrLookupFailure, err)
			}
			if cand != nil {
				return r.attachBlock(ctx, span, cand, ReasonLocationMatched), nil
			}
			nameMissReason = ReasonNoFilepathMatch
		}
		// If lineno is missing / unparseable we cannot use the
		// filepath rung at all (the §8.6 mapping pairs them) —
		// keep the rung-1 miss reason if one was recorded.
	}

	// ----- Rung 3: drop and count -------------------------
	if r.metrics != nil {
		r.metrics.IncUnresolved(span.RepoID)
	}
	if r.logger != nil {
		r.logger.Debug("spaningestor.unresolved",
			slog.String("repo_id", span.RepoID),
			slog.String("trace_id", span.TraceID),
			slog.String("span_id", span.SpanID),
			slog.String("reason", nameMissReason.String()))
	}
	return Resolution{Status: StatusUnresolved, Reason: nameMissReason}, nil
}

// attachBlock attempts the Stage 4.1 second sub-step: after the
// Method is found, use `code.lineno` against the ingested Block
// boundaries.  Returns a `Resolution` carrying either the
// Method-and-Block (when a Block covers the line) or
// Method-only (per §8.6: "If no Block matches, attach the
// observation to the Method Node").
//
// `methodReason` is the rung-side classification (NameMatched
// or LocationMatched).  The block-side outcome lands in
// `Resolution.BlockOutcome`; the two dimensions stay
// orthogonal so the caller sees both pieces of information
// regardless of which rung produced the Method.
func (r *Resolver) attachBlock(
	ctx context.Context,
	span Span,
	method *MethodCandidate,
	methodReason ResolutionReason,
) Resolution {
	res := Resolution{Status: StatusMethod, Method: method, Reason: methodReason}
	raw, has := span.Attributes[AttrCodeLineno]
	if !has || strings.TrimSpace(raw) == "" {
		res.BlockOutcome = BlockOutcomeNoLineno
		return res
	}
	lineno, ok := parseLineno(raw)
	if !ok {
		res.BlockOutcome = BlockOutcomeLinenoUnparseable
		return res
	}
	block, err := r.lookup.LookupBlockForMethod(ctx, method.NodeID, lineno)
	if err != nil {
		// Block lookup failure should not invalidate the
		// already-resolved Method.  Per §8.6 the fallback on a
		// Block miss is "attach to the Method" — a backend
		// failure on the block lookup is operationally
		// identical to a "no block matches" miss for the
		// observation's destination.  We log so the operator
		// can see backend errors but do not propagate.
		if r.logger != nil {
			r.logger.Warn("spaningestor.block_lookup_failed",
				slog.String("repo_id", span.RepoID),
				slog.String("method_node_id", method.NodeID),
				slog.Int("lineno", lineno),
				slog.String("error", err.Error()))
		}
		res.BlockOutcome = BlockOutcomeLookupFailed
		return res
	}
	if block == nil {
		res.BlockOutcome = BlockOutcomeOutsideBlock
		return res
	}
	res.Status = StatusBlock
	res.Block = block
	res.BlockOutcome = BlockOutcomeMatched
	return res
}

// chooseMethod implements the rung-1 disambiguation policy:
//
//	0 candidates                 → (nil, ReasonNoNameMatch)
//	1 candidate, sig empty       → (cand, ReasonNameMatched)
//	1 candidate, sig set         → see signature-match policy below
//	>1 candidates, sig empty     → (nil, ReasonAmbiguousName)
//	>1 candidates, sig set       → filter by ParamSignature equality;
//	                               1 survivor → (it, ReasonNameMatched);
//	                               0 / >1 survivors → (nil, ReasonSignatureMismatch / ReasonAmbiguousName)
//
// Signature-match policy on a unique candidate
// --------------------------------------------
// When `sig` is set and the unique candidate carries a non-empty
// `ParamSignature`, we require normalized equality — a mismatch
// falls through to the filepath rung.  This is the rubber-duck
// blocker fix: silently accepting a contradictory unique candidate
// would pollute observation aggregates with a wrong destination,
// which is operationally worse than dropping the span and
// surfacing it as unresolved (the operator at least sees the
// `span_unresolved_total` signal and can debug).  When the
// candidate has an empty `ParamSignature` (the graph could not
// extract one), accepting the unique candidate is correct — the
// signature attribute is the disambiguator-of-last-resort, not a
// blocker on its own.
func chooseMethod(cands []MethodCandidate, sig string) (*MethodCandidate, ResolutionReason) {
	if len(cands) == 0 {
		return nil, ReasonNoNameMatch
	}
	if len(cands) == 1 {
		only := cands[0]
		if sig == "" {
			return &only, ReasonNameMatched
		}
		if only.ParamSignature == "" {
			return &only, ReasonNameMatched
		}
		if signatureMatches(only.ParamSignature, sig) {
			return &only, ReasonNameMatched
		}
		return nil, ReasonSignatureMismatch
	}
	// len(cands) > 1
	if sig == "" {
		return nil, ReasonAmbiguousName
	}
	var matched []MethodCandidate
	for _, c := range cands {
		if c.ParamSignature == "" {
			continue
		}
		if signatureMatches(c.ParamSignature, sig) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 0:
		return nil, ReasonSignatureMismatch
	case 1:
		only := matched[0]
		return &only, ReasonNameMatched
	default:
		return nil, ReasonAmbiguousName
	}
}

// signatureMatches reports whether `candidate` (a Method's
// ParamSignature, e.g. `int,string`) matches the OTel
// `code.signature` attribute (which may be the bare params
// `(int, string)`, the method+params `bar(int, string)`, or the
// fully-qualified `Foo.bar(int, string)`).  The match is
// performed on the normalized param-only form so all three OTel
// shapes converge to the same comparison key.
//
// Normalization MUST match the canonical-signature form produced
// by `ast.NormalizeSignature` (the §9.7 / §9.9 fingerprint
// invariant) — `dispatcher.methodSignature` runs `params`
// through `ast.NormalizeSignature` before minting a Method's
// canonical signature, so the candidate side stored in the
// graph is whitespace-stripped around `,`/`(`/`)`/`<`/`>` etc.
// If `normalizeParams` only collapsed whitespace runs (without
// removing the surrounding space) the observed `(int, string)`
// would normalize to `int, string` and never compare equal to
// the canonical `int,string` — overload disambiguation would
// silently fail on every multi-param method.  We therefore
// delegate to the same normalizer the dispatcher uses, so the
// two sides cannot drift.
func signatureMatches(candidate, observed string) bool {
	return normalizeParams(candidate) == normalizeParams(observed)
}

// normalizeParams extracts the bare parameter list from an
// OTel `code.signature` string (which may be `int`, `(int)`,
// `bar(int)`, or `Foo.bar(int, string)`) and runs it through
// the repository's canonical normalizer so it matches the
// stored Method canonical-signature parameter form.
//
// Steps:
//  1. TrimSpace.
//  2. If the trimmed value ends with `)`, locate the matching
//     OUTERMOST `(` via a right-to-left balanced-paren scan
//     and strip the `name(` prefix and trailing `)`.  This
//     collapses `Foo.bar(int, string)` → `int, string`,
//     `(int)` → `int`, and — critically for non-Go languages
//     where parameter TYPES themselves can carry parens —
//     keeps function-typed parameters intact (e.g. the
//     TypeScript/Python shapes `bar(Func(int), string)` and
//     `handler(callback: (x: int) => void, flag: bool)`).
//     A naïve `strings.LastIndex(s, "(")` would lock onto the
//     INNERMOST `(` and yield `int), string` /
//     `x: int) => void, flag: bool` respectively, silently
//     mangling the comparison key for every overload that
//     uses a callback-typed parameter.
//  3. If step 2 cannot find a balanced pair (e.g. an
//     unbalanced legacy input like `foo(int`), fall back to
//     the original LAST-`(` heuristic so historical inputs
//     keep producing the same parameter slice rather than
//     silently regressing to "no params extracted".
//  4. Run the result through `ast.NormalizeSignature`, which
//     (a) collapses Unicode whitespace runs to single ASCII
//     spaces, (b) strips spaces adjacent to canonical
//     punctuation (`,`/`(`/`)`/`[`/`]`/`{`/`}`/`<`/`>`/`:`/`;`),
//     and (c) trims.  This is the SAME normalizer
//     `dispatcher.methodSignature` invokes on the params side
//     of every canonical Method signature, so the candidate's
//     `ParamSignature` (already canonical) and the observed
//     `code.signature` produce byte-identical comparison keys.
func normalizeParams(s string) string {
	s = strings.TrimSpace(s)
	if open := matchingOpenParen(s); open >= 0 {
		// Balanced trailing `)` — strip `name(` … `)`.
		s = s[open+1 : len(s)-1]
	} else if i := strings.LastIndex(s, "("); i >= 0 {
		// Unbalanced / no trailing `)` — preserve the
		// pre-fix behaviour so historical inputs like
		// `foo(int` (no closing paren) keep yielding the
		// same `int` rather than the raw `foo(int`.
		s = s[i+1:]
		if j := strings.LastIndex(s, ")"); j >= 0 {
			s = s[:j]
		}
	}
	return astpkg.NormalizeSignature(s)
}

// matchingOpenParen returns the byte index of the `(` that
// matches the trailing `)` in `s` using a right-to-left
// balanced-paren scan, or -1 when `s` does not end with `)`
// or the parens are unbalanced (no zero-depth `(` reached).
//
// This is the structural fix for the nested-paren parameter
// case: callers want the OUTERMOST `(` introducing the param
// list, not the innermost one — `strings.LastIndex(s, "(")`
// would return the innermost.  The scan ignores `(` and `)`
// embedded in string / character literals because OTel
// `code.signature` is a TYPE signature, not an expression
// (no string defaults arrive on this path); guarding against
// quoted parens would be over-engineering for the contract
// the dispatcher actually emits.
func matchingOpenParen(s string) int {
	n := len(s)
	if n == 0 || s[n-1] != ')' {
		return -1
	}
	depth := 0
	for i := n - 1; i >= 0; i-- {
		switch s[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// parseLineno parses an OTel `code.lineno` attribute (a string
// in our normalized representation, even though OTLP transports
// it as an int) into a positive integer.  Returns ok=false for
// empty / non-numeric / non-positive values; the resolver treats
// any of those as "no usable lineno" rather than as a backend
// error.
func parseLineno(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// normalizeFilepath converts a `code.filepath` attribute into the
// forward-slash, leading-`./`-free form the Lookup contract
// requires.  Absolute paths are passed through unmodified so the
// Lookup implementation can either strip the configured repo
// root prefix itself or reject the span; this keeps the resolver
// from having to know per-deployment workspace layout.
func normalizeFilepath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// path.Clean does not touch backslashes; do that first.
	raw = strings.ReplaceAll(raw, "\\", "/")
	cleaned := path.Clean(raw)
	if cleaned == "." {
		// Clean turns `./` into `.`; treat that as empty so
		// the filepath rung doesn't attempt to look up the
		// repo root.
		return ""
	}
	return cleaned
}
