// Package churn is the writer-side adapter for the
// `ingest.churn` external webhook payload (architecture Sec
// 3.12, Sec 4.4 lines 778-790). It does NOT implement the
// HTTP webhook handler itself -- the handler lives in a later
// stage of the External Metric Ingest Webhook (Sec 3.12).
// What this package owns:
//
//  1. The canonical Go-side struct shape of the payload
//     ([Payload], [PayloadRow]) the webhook handler will
//     unmarshal CI POST bodies into. The handler is the only
//     place a payload enters the process; downstream
//     consumers (the Metric Ingestor's churn sweep in
//     `internal/metric_ingestor`) consume the validated
//     in-memory form, not the wire bytes.
//
//  2. The [Hydrator] that turns a [Payload] into
//     `[]HydratedChurnRow` -- the writer-ready records the
//     `modification_count_in_window` materialiser
//     ([github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/materialisers])
//     consumes plus the durable `scope_id` UUID the Metric
//     Ingestor stamps on the emitted `metric_sample` row.
//
// # Per-row SHA contract (architecture Sec 4.4)
//
// The `ingest.churn` payload is `sha_binding='per_row'`: each
// [PayloadRow] carries its own SHA. This is the OPPOSITE of
// the foundation-tier `kind='full'` scan binding where the
// ScanRun's `to_sha` is the single authoritative commit. The
// hydrator MUST preserve per-row SHA fidelity so the
// downstream `MetricSample.sha` column matches the commit that
// actually touched the scope -- joining hot-cold cycles in
// `velocity_trend` (system-tier row 3) depends on this.
//
// # Identity model
//
// Each [PayloadRow] reports a `(repo_id, sha, file_path,
// modified_at)` quadruple. The hydrator resolves
// `file_path` to a durable `(scope_id, ScopeRef, ScopeKey)`
// via a [ScopeResolver]. In the foundation production path
// the resolver is backed by the `scope_binding` table (the
// row was minted by the AST adapter during the foundation
// scan at the same commit); in unit tests the in-memory
// [MapScopeResolver] suffices.
//
// # File-scope only at this stage
//
// Stage 2.6 emits FILE-scope rows only. Architecture Sec
// 1.4.1 row 12 also lists `method` as an allowed scope_kind
// for `modification_count_in_window`, but resolving a churn
// payload's per-row line attribution to method-level scopes
// requires AST-line-attribution which lands in a later stage
// (`phase-ast-adapter-and-foundation-tier-compute/stage-method-scope-line-attribution`,
// not yet built). The hydrator emits one [HydratedChurnRow]
// per payload row at `scope_kind='file'`; a TODO points to
// the future expansion.
package churn

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/materialisers"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// ScanRunKindExternalPerRow is the canonical `ScanRun.kind`
// value for a STANDALONE churn-only batch (the `ingest.churn`
// webhook arrived without a foundation scan in flight --
// architecture Sec 4.4 line 782, implementation-plan Stage
// 3.2 line 290).
//
// # Accepted parent ScanRun kinds (post iter-3)
//
// The Metric Ingestor's churn-sweep validator
// ([github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor.AllowedScanRunKinds])
// ACCEPTS THREE kinds (NOT just this one):
//
//   - `full`     -- a foundation-tier initial scan; the
//     [github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor.Ingestor]
//     coordinator dispatches the AST recipes FIRST, then
//     invokes the churn sweep INLINE inside the SAME
//     ScanRun so the active-row uniqueness invariant (G2)
//     lands atomically across both writers.
//   - `delta`    -- a foundation-tier incremental scan;
//     same rationale as `full`.
//   - `external_per_row` (this const) -- the standalone
//     webhook path with no AST work attached. The
//     [Ingestor] runs the churn sweep ONLY.
//
// REJECTED kinds:
//
//   - `external_single` (the coverage / defect single-SHA
//     binding) -- the per-row-SHA semantics of
//     `modification_count_in_window` would corrupt those
//     runs' active-row state.
//   - `retract` (Sec 5.7 tombstone) -- a retract scan
//     deletes active rows, not writes new ones.
//
// The literal is re-exported as
// `metric_ingestor.ScanRunKindExternalPerRow` and the two
// MUST remain string-equal -- a sweep-test pins the
// invariant.
const ScanRunKindExternalPerRow = "external_per_row"

// Validation errors. Surfaced as wrapped errors (NOT panics)
// because a malformed payload at the webhook boundary is a
// caller-induced runtime fault, not a writer-layer bug --
// `errors.Is` lets the HTTP handler stage map them to
// `400 Bad Request` responses without parsing strings.
var (
	// ErrEmptyRepoID is returned when [Payload.RepoID] is
	// the zero UUID. A zero `repo_id` always indicates an
	// uninitialised caller value; legitimate clean-code rows
	// reference a `repo.repo_id` allocated via
	// `gen_random_uuid()` which never returns zero.
	ErrEmptyRepoID = errors.New("churn: payload RepoID is the zero UUID")
	// ErrEmptyRows is returned when [Payload.Rows] is empty
	// or nil. A churn payload with no rows would be a no-op;
	// surface it so the operator can fix the publisher.
	ErrEmptyRows = errors.New("churn: payload Rows is empty")
	// ErrEmptySHA is returned when a [PayloadRow.SHA] is the
	// empty string. The per-row-SHA contract (arch Sec 4.4)
	// requires every row to carry its own commit identity.
	ErrEmptySHA = errors.New("churn: payload row has empty SHA")
	// ErrInvalidSHA is returned when a [PayloadRow.SHA] is
	// non-empty but does not match the canonical 40-character
	// hex commit-SHA shape (evaluator iter-4 #3). The pattern
	// `^[0-9a-fA-F]{40}$` accepts both lowercase (Git's
	// default) and uppercase hex; whitespace-padded,
	// truncated, or non-hex strings are REJECTED so a
	// malformed value cannot flow into
	// `MetricSampleRecord.SHA` and on to the active-row
	// dedupe key. The wrap carries the offending string so
	// the HTTP handler stage can return a structured 400
	// without parsing the error text.
	ErrInvalidSHA = errors.New("churn: payload row SHA is not a 40-character hex commit SHA")
	// ErrEmptyFilePath is returned when a [PayloadRow.FilePath]
	// is the empty string. The hydrator resolves file_path to
	// a durable scope_id; an empty path cannot be resolved.
	ErrEmptyFilePath = errors.New("churn: payload row has empty FilePath")
	// ErrZeroModifiedAt is returned when a
	// [PayloadRow.ModifiedAt] is the zero time.Time. The
	// materialiser's window math (`now - window_days * 24h`)
	// cannot meaningfully bucket a zero-valued timestamp.
	//
	// Renamed from `ErrZeroCommitterDate` in iter 6 to align
	// with tech-spec Sec 4.11 / Sec 8.5 which pin the wire
	// field name as `modified_at` (the Git-side term
	// `committer_date` was an internal-vocabulary leak).
	ErrZeroModifiedAt = errors.New("churn: payload row has zero ModifiedAt")
	// ErrScopeResolutionFailed wraps the underlying error
	// from a [ScopeResolver]. The hydrator stops at the
	// first unresolvable row -- partial writes would violate
	// the writer-ownership invariant (a Sweep must emit ALL
	// or NONE of the per-payload draft set).
	ErrScopeResolutionFailed = errors.New("churn: scope resolution failed")
)

// Payload is the canonical in-process form of an
// `ingest.churn` POST body (architecture Sec 4.4 lines
// 778-790). The wire-format is intentionally NOT pinned here
// (JSON vs Protobuf is the webhook handler's call); only the
// struct shape callers consume is canonical.
//
// JSON tags are populated so a future webhook handler can
// `json.Unmarshal` straight into this struct without an
// intermediate DTO; the field names match the CI-side
// publisher contract pinned in the operator runbook.
type Payload struct {
	// RepoID is the `clean_code.repo.repo_id` the payload
	// targets. The webhook handler resolves the caller's
	// per-source signing key (architecture Sec 3.12) to this
	// UUID before invoking the hydrator; the hydrator itself
	// trusts the resolved value.
	RepoID uuid.UUID `json:"repo_id"`
	// Rows is the per-commit / per-file churn record set.
	// Each row carries its own SHA (arch Sec 4.4 line 781:
	// "each row has its own SHA").
	Rows []PayloadRow `json:"rows"`
}

// PayloadRow is one churn record: "commit `sha` touched file
// `file_path` on date `modified_at`". The `Author` field
// is reserved for future `knowledge_index` (system-tier row
// 7) -- the materialiser ignores it for Stage 2.6.
type PayloadRow struct {
	// SHA is the 40-char commit SHA (per-row binding, arch
	// Sec 4.4 line 781). The materialiser dedupes by this
	// value so multi-hunk commits count as one.
	SHA string `json:"sha"`
	// FilePath is the repo-relative path of the touched file
	// (e.g. `services/clean-code/internal/foo.go`). The
	// [Hydrator] passes this verbatim to its [ScopeResolver]
	// to mint the durable `scope_id`.
	FilePath string `json:"file_path"`
	// ModifiedAt is the commit's modification timestamp in
	// UTC (the Git `committer_date` for a foundation-tier
	// publisher, but the wire-format field is named
	// `modified_at` to match tech-spec Sec 4.11 / Sec 8.5
	// which canonicalises the field across all four ingest
	// verbs).
	//
	// The materialiser's window math is the only consumer.
	// The webhook handler MUST normalise wire timestamps to
	// UTC before constructing the payload.
	//
	// # Wire-format rename in iter 6
	//
	// The struct field + JSON tag were renamed from
	// `CommitterDate`/`committer_date` to `ModifiedAt`/`modified_at`
	// in iter 6 to align with the tech-spec wire contract
	// (evaluator iter-5 #1). The struct decoder uses
	// `DisallowUnknownFields`, so a publisher posting the
	// legacy `committer_date` key will be rejected with a
	// structured 400 -- the wire contract is now strict
	// canonical, and the runbook documents the migration.
	ModifiedAt time.Time `json:"modified_at"`
	// Author is the commit author identity (e.g.
	// `alice@example.com`). Reserved for `knowledge_index`;
	// the Stage 2.6 materialiser ignores it. JSON-omitempty
	// so a publisher that doesn't have author data does not
	// have to send an empty string.
	Author string `json:"author,omitempty"`
}

// Validate returns nil iff the payload satisfies every
// structural contract the hydrator depends on. Validation
// errors are wrapped on the corresponding sentinel
// (`errors.Is(err, ErrEmptyRepoID)` etc.) so the HTTP
// handler stage can map each to a structured response
// without parsing strings.
//
// Future-dated `ModifiedAt` rows are NOT rejected here
// -- the materialiser's window math drops them with a
// clock-skew defence (`ModifiedAt > now` => ignored). A
// strict-mode caller can implement its own per-row filter
// before invoking the hydrator if it wants to fail-loud
// instead of drop-silently.
func (p *Payload) Validate() error {
	if p == nil {
		return errors.New("churn: payload is nil")
	}
	if p.RepoID == uuid.Nil {
		return ErrEmptyRepoID
	}
	if len(p.Rows) == 0 {
		return ErrEmptyRows
	}
	for i := range p.Rows {
		if err := validateRow(&p.Rows[i]); err != nil {
			return fmt.Errorf("rows[%d]: %w", i, err)
		}
	}
	return nil
}

// shaRegex is the strict canonical pattern for a commit SHA:
// exactly 40 hexadecimal characters, no leading/trailing
// whitespace, case-insensitive (Git emits lowercase but
// upstream consumers MAY upper-case). Used by [validateRow] to
// satisfy the iter-4 #3 contract: a malformed SHA cannot
// silently flow into `MetricSampleRecord.SHA`.
var shaRegex = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// validateRow checks a single [PayloadRow] for structural
// validity. Empty SHAs, malformed (non-40-hex) SHAs, empty
// file paths, and zero-valued timestamps are rejected;
// future-dated timestamps are PERMITTED (the materialiser
// drops them downstream).
func validateRow(r *PayloadRow) error {
	if strings.TrimSpace(r.SHA) == "" {
		return ErrEmptySHA
	}
	if !shaRegex.MatchString(r.SHA) {
		return fmt.Errorf("%w (got %q)", ErrInvalidSHA, r.SHA)
	}
	if strings.TrimSpace(r.FilePath) == "" {
		return ErrEmptyFilePath
	}
	if r.ModifiedAt.IsZero() {
		return ErrZeroModifiedAt
	}
	return nil
}

// HydratedChurnRow pairs a writer-layer [materialisers.ChurnRow]
// with the durable `scope_id` UUID the Metric Ingestor stamps
// onto the emitted `metric_sample` row. The materialiser
// itself is identity-agnostic (it groups by opaque
// [materialisers.ChurnRow.ScopeKey]); the hydrator threads
// the resolved UUID alongside so the Sweep can map each
// emitted draft back to its scope_id without re-resolving.
//
// Per the rubber-duck design review (iter 2): keeping the
// UUID OFF [materialisers.ChurnRow] preserves the
// materialiser's purity (no persistence concerns) and lets
// future non-DB materialiser callers (CLI export, dry-run)
// run without a scope_binding fixture.
type HydratedChurnRow struct {
	// Row is the writer-layer record the materialiser
	// consumes. [materialisers.ChurnRow.ScopeKey] is set to
	// the resolved scope_id UUID's canonical 36-char string;
	// this is the durable identity the materialiser dedupes
	// scopes by.
	Row materialisers.ChurnRow
	// ScopeID is the durable UUID minted by the
	// [ScopeResolver]. The Sweep uses it to stamp
	// `metric_sample.scope_id`.
	ScopeID uuid.UUID
}

// ScopeResolver translates a `(repo_id, file_path)` pair to
// the durable `scope_id` and the materialiser-layer
// [recipes.ScopeRef] representative. The production
// implementation reads the `clean_code.scope_binding` table
// (the AST adapter minted the row during the foundation
// scan at the same commit -- architecture Sec 4.1 step 5).
//
// The interface is small on purpose: it does NOT take a SHA
// argument because the file's scope identity is SHA-stable
// (G2: same canonical signature at two SHAs yields the same
// scope_id per arch Sec 5.2.3 line 1044). A future
// per-method resolver will need richer inputs (the commit's
// AST + line attribution) but is out of scope for Stage 2.6.
type ScopeResolver interface {
	// ResolveFile returns the durable `scope_id` and the
	// materialiser-layer [recipes.ScopeRef] for the file at
	// `repoID + filePath`. Returns
	// ([uuid.Nil], _, error) when the path has no
	// `scope_binding` row -- the hydrator wraps the error in
	// [ErrScopeResolutionFailed].
	ResolveFile(ctx context.Context, repoID uuid.UUID, filePath string) (scopeID uuid.UUID, ref recipes.ScopeRef, err error)
}

// MapScopeResolver is an in-memory [ScopeResolver] for tests
// and for the early skeletal Metric Ingestor wiring. Keyed
// by `(repo_id.String() + "|" + file_path)`; missing keys
// are reported as a structured error wrapped in
// [ErrScopeResolutionFailed].
//
// The constructor minted UUID is computed via
// [scope.DeriveScopeID] so test fixtures mirror the
// production identity model.
type MapScopeResolver struct {
	entries map[string]mapResolverEntry
}

type mapResolverEntry struct {
	scopeID uuid.UUID
	ref     recipes.ScopeRef
}

// NewMapScopeResolver returns an empty [MapScopeResolver].
// Callers add entries via [MapScopeResolver.Add] for each
// file the payload references.
func NewMapScopeResolver() *MapScopeResolver {
	return &MapScopeResolver{entries: map[string]mapResolverEntry{}}
}

// Add registers a `(repoID, filePath) -> (scopeID, ScopeRef)`
// mapping. Re-adding the same key OVERWRITES; a duplicate is
// a test fixture bug worth surfacing but the resolver is the
// lenient version (the production reader is the strict one).
func (r *MapScopeResolver) Add(repoID uuid.UUID, filePath string, scopeID uuid.UUID, ref recipes.ScopeRef) {
	r.entries[mapKey(repoID, filePath)] = mapResolverEntry{scopeID: scopeID, ref: ref}
}

// ResolveFile implements [ScopeResolver].
func (r *MapScopeResolver) ResolveFile(_ context.Context, repoID uuid.UUID, filePath string) (uuid.UUID, recipes.ScopeRef, error) {
	e, ok := r.entries[mapKey(repoID, filePath)]
	if !ok {
		return uuid.Nil, recipes.ScopeRef{}, fmt.Errorf("MapScopeResolver: no entry for repo=%s file=%q", repoID, filePath)
	}
	return e.scopeID, e.ref, nil
}

func mapKey(repoID uuid.UUID, filePath string) string {
	return repoID.String() + "|" + filePath
}

// autoResolverNamespace is the UUIDv5 namespace
// [AutoMapScopeResolver] uses to mint deterministic scope_ids
// from `(repo_id, file_path)`. Pinned as a stable UUID literal
// so a scaffold-mode service that POSTs the SAME (repo,
// file_path) twice yields the SAME scope_id (the active-row
// invariant requires identity stability across calls; the
// Phase 3.2 PG-backed resolver derives it from
// `scope.DeriveScopeID` against the warmed `scope_binding`
// table -- the auto-resolver is the scaffold equivalent).
var autoResolverNamespace = uuid.Must(uuid.FromString("9e9c4f9e-7c2c-5f9e-9c2c-5f9e9c2c5f9e"))

// AutoMapScopeResolver is a [ScopeResolver] that mints a
// DETERMINISTIC `scope_id` for every `(repo_id, file_path)`
// pair via UUIDv5 of `repo_id.String() + "|" + file_path`. It
// is the production scaffold resolver wired by
// `cmd/clean-coded/main.go` in iter 5: the in-memory
// [MapScopeResolver] cannot service the HTTP webhook (it
// requires pre-registration of every file the payload
// references), so the webhook handler needs a resolver that
// works for ARBITRARY incoming file paths.
//
// # Why UUIDv5 (not v4)
//
// Active-row uniqueness (architecture Sec 5.2.2) requires the
// SAME (repo, scope_kind, file_path) to map to the SAME
// scope_id across calls. UUIDv4 would mint a fresh random ID
// each invocation; UUIDv5 hashes the name input and returns
// the same UUID for the same input. Two POSTs of the same
// payload therefore produce ONE active row, not two with
// different scope_ids.
//
// # Relationship to scope.DeriveScopeID
//
// The Phase 3.2 PG-backed resolver derives scope_ids via
// [scope.DeriveScopeID] over the canonical signature read
// from the `scope_binding` table. The auto-resolver is a
// shortcut for scaffold mode -- the hash input is just
// `(repo, file_path)` because no canonical-signature data is
// available before the foundation scan runs. The two paths
// produce DIFFERENT scope_ids for the same logical file (the
// auto-resolver's IDs are not portable to Phase 3.2); this is
// acceptable because scaffold-mode rows are in-memory only
// and do not persist across the upgrade.
//
// # Concurrent use
//
// The struct holds no mutable state; every Resolve call is a
// pure function. Safe for concurrent use.
type AutoMapScopeResolver struct{}

// NewAutoMapScopeResolver returns an [AutoMapScopeResolver].
// Construction is trivial (no state) but the constructor is
// exported for compositional symmetry with [NewMapScopeResolver].
func NewAutoMapScopeResolver() *AutoMapScopeResolver {
	return &AutoMapScopeResolver{}
}

// ResolveFile implements [ScopeResolver] by deterministically
// minting a UUIDv5 from `(repo_id, file_path)`. The
// [recipes.ScopeRef] is filled with `kind=file`,
// `QualifiedName=path`, `Path=path`, and `LocalID` set to the
// minted UUID's canonical 36-char string (matching the
// post-hydrator invariant pinned by the iter-2 design).
func (r *AutoMapScopeResolver) ResolveFile(_ context.Context, repoID uuid.UUID, filePath string) (uuid.UUID, recipes.ScopeRef, error) {
	if repoID == uuid.Nil {
		return uuid.Nil, recipes.ScopeRef{}, fmt.Errorf("AutoMapScopeResolver: repo_id is the zero UUID")
	}
	if filePath == "" {
		return uuid.Nil, recipes.ScopeRef{}, fmt.Errorf("AutoMapScopeResolver: file_path is empty")
	}
	sid := uuid.NewV5(autoResolverNamespace, repoID.String()+"|"+filePath)
	return sid, recipes.ScopeRef{
		LocalID:       sid.String(),
		Kind:          scope.KindFile,
		QualifiedName: filePath,
		Path:          filePath,
	}, nil
}

// Hydrator translates a validated [Payload] into the
// writer-ready [HydratedChurnRow] slice the Metric Ingestor's
// churn-sweep feeds to the materialiser.
//
// The Hydrator is stateless beyond its [ScopeResolver]
// dependency; the resolver itself MAY be backed by a
// per-request connection / cache / read-through pattern (the
// production `scope_binding` reader). Two concurrent
// [Hydrator.Hydrate] calls on the same hydrator are safe iff
// the underlying resolver is safe for concurrent use.
type Hydrator struct {
	resolver ScopeResolver
}

// NewHydrator returns a [Hydrator] bound to `resolver`.
// PANICS when `resolver == nil` -- a hydrator without a
// resolver cannot produce a single row, so the misconfig is
// a wiring bug that should fail loudly at composition root
// time.
func NewHydrator(resolver ScopeResolver) *Hydrator {
	if resolver == nil {
		panic("churn: NewHydrator received nil ScopeResolver")
	}
	return &Hydrator{resolver: resolver}
}

// Hydrate validates the payload, resolves every row to a
// durable `(scope_id, ScopeRef)` via [ScopeResolver], and
// returns the writer-ready [HydratedChurnRow] slice in
// PAYLOAD ORDER (the materialiser does its own sort on
// output; preserving payload order here keeps the per-row
// progress meaningful for debug logs).
//
// On the FIRST resolution failure, Hydrate returns
// ([]HydratedChurnRow{nil}, error) -- partial output is
// suppressed so the caller observes "no rows written" rather
// than a partially-written sweep that would violate writer
// ownership atomicity. The Metric Ingestor's writer is
// expected to use a batch INSERT inside the parent ScanRun,
// so a hydrate error short-circuits before any DB write.
//
// # Stage 2.6 file-scope-only behaviour
//
// Every emitted [HydratedChurnRow] is at `scope_kind='file'`.
// Architecture Sec 1.4.1 row 12 also lists `method`, but
// AST-line-attribution (the prerequisite for resolving a
// per-row file_path to a method-scope) is a later workstream.
// TODO Stage 4: emit method-scope rows when AST-line-
// attribution lands; the Hydrator's API stays compatible
// because the additional rows are appended to the existing
// per-row slice (no caller change required).
func (h *Hydrator) Hydrate(ctx context.Context, payload *Payload) ([]HydratedChurnRow, error) {
	if err := payload.Validate(); err != nil {
		return nil, err
	}
	out := make([]HydratedChurnRow, 0, len(payload.Rows))
	for i := range payload.Rows {
		r := &payload.Rows[i]
		scopeID, ref, err := h.resolver.ResolveFile(ctx, payload.RepoID, r.FilePath)
		if err != nil {
			return nil, fmt.Errorf("%w: %v (rows[%d] file_path=%q)", ErrScopeResolutionFailed, err, i, r.FilePath)
		}
		if scopeID == uuid.Nil {
			return nil, fmt.Errorf("%w: ScopeResolver returned the zero UUID (rows[%d] file_path=%q)", ErrScopeResolutionFailed, i, r.FilePath)
		}
		// Defence-in-depth: the materialiser's
		// allowedScopeKinds is {file, method}, but the
		// hydrator only emits file-scope rows at Stage 2.6.
		// Reject a resolver that hands back a non-file
		// kind so a buggy fixture cannot smuggle e.g. a
		// `class` row through.
		if ref.Kind != scope.KindFile {
			return nil, fmt.Errorf("%w: hydrator emits file-scope rows only at Stage 2.6, got kind=%q (rows[%d] file_path=%q)", ErrScopeResolutionFailed, ref.Kind, i, r.FilePath)
		}
		// Stamp the durable scope_id onto ScopeRef.LocalID so
		// the materialiser-emitted draft round-trips back to
		// the scope_id without an out-of-band lookup. This is
		// the documented purpose of LocalID per
		// `recipes.ScopeRef` Sec "The Metric Ingestor rewrites
		// it to a durable scope_id UUID via DeriveScopeID":
		// the hydrator IS the Metric Ingestor's churn-path
		// rewrite step. Tests that supplied LocalID via the
		// resolver get the value overwritten -- the
		// invariant is "LocalID == scope_id UUID string at
		// hydrator output".
		ref.LocalID = scopeID.String()
		out = append(out, HydratedChurnRow{
			Row: materialisers.ChurnRow{
				// ScopeKey is the durable scope_id UUID
				// string -- the materialiser groups by this
				// opaque value, so two payload rows for the
				// same file (but, say, different LocalIDs if
				// a future caller misuses ScopeRef) collapse
				// to one scope correctly.
				ScopeKey:   scopeID.String(),
				Scope:      ref,
				SHA:        r.SHA,
				ModifiedAt: r.ModifiedAt.UTC(),
			},
			ScopeID: scopeID,
		})
	}
	return out, nil
}

// Rows extracts the materialiser-layer [materialisers.ChurnRow]
// slice from a hydrated slice. Convenience helper for the
// Metric Ingestor's churn-sweep, which calls
// `materialiser.Materialise(churn.Rows(hydrated))` and then
// joins back by ScopeKey to recover the durable UUID.
func Rows(hydrated []HydratedChurnRow) []materialisers.ChurnRow {
	out := make([]materialisers.ChurnRow, len(hydrated))
	for i, h := range hydrated {
		out[i] = h.Row
	}
	return out
}

// ScopeIDByKey returns a `scope_key -> scope_id` lookup map
// for a hydrated slice. The Metric Ingestor's churn-sweep
// uses it to recover the durable UUID for each
// materialiser-emitted draft (the draft itself only carries
// the [recipes.ScopeRef] LocalID + canonical signature). The
// returned map's keys are exactly the
// [materialisers.ChurnRow.ScopeKey] values the materialiser
// grouped by.
func ScopeIDByKey(hydrated []HydratedChurnRow) map[string]uuid.UUID {
	out := make(map[string]uuid.UUID, len(hydrated))
	for _, h := range hydrated {
		out[h.Row.ScopeKey] = h.ScopeID
	}
	return out
}
