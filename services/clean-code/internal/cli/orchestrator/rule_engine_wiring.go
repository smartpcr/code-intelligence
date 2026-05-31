// -----------------------------------------------------------------------
// <copyright file="rule_engine_wiring.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator

import (
	"fmt"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// SampleIDNamespacePrefix is the literal namespace seed
// [MintSampleID] prepends to the
// `(repoID, headSHA, scopeID, metricKind, metricVersion)`
// pre-image before hashing through UUID-v5. Pinning a CLI-
// specific prefix means a CLI-minted `sample_id` can never
// collide with an Ingestor-minted production row even if
// the two layers happen to converge on the same coordinate
// tuple. The prefix mirrors the
// [repocontext.RepoIDNamespaceNamePrefix] convention (a
// human-readable, slash-terminated source-of-truth literal
// so cross-component drift is loud rather than silent).
const SampleIDNamespacePrefix = "cleanc.local-sample/"

// BuildSamples converts the Stage 2.2
// [recipes.MetricSampleDraft] output into the
// [rule_engine.Sample] rows the engine consumes per
// `architecture.md` Sec 4.4 field mapping:
//
//   - `dsl.Sample.RepoID`, `dsl.Sample.SHA` from [repoCtx].
//   - `dsl.Sample.ScopeID`, `dsl.Sample.ScopeKind` resolved
//     from [scopeIDs] (the orchestrator's
//     [Result.ScopeIDs] map keyed by `(path, local_id)`).
//   - `dsl.Sample.MetricKind`, `dsl.Sample.MetricVersion`,
//     `dsl.Sample.Value` copied from the draft.
//   - `dsl.Sample.Pack`, `dsl.Sample.Source` flattened to
//     their string forms.
//   - `dsl.Sample.HasValue = true` (Stage 2.2 recipes never
//     emit a draft without a meaningful value; see
//     `recipes/recipe.go` comment "if an input is missing
//     the row is not written, not stamped degraded").
//   - `dsl.Sample.SampleID` is a deterministic UUID-v5 (see
//     [MintSampleID]) so re-runs over the same checkout
//     yield byte-identical rows per tech-spec C11.
//   - [rule_engine.Sample.ScopeSignature] from the matching
//     [scopebinding.Table] entry (architecture Sec 4.4
//     line 752).
//
// Drafts whose `(path, local_id)` does NOT resolve through
// [scopeIDs] are silently skipped: the orchestrator already
// recorded a [SkipReasonScopeBindingError] row for that
// scope, and re-surfacing it here would double-report. The
// same applies to drafts whose ScopeID resolves but whose
// corresponding [scopebinding.Table] row is missing -- the
// binding's signature is the engine's override-match key,
// and an empty signature would silently match a `*` glob
// (per the rule_engine.Sample.ScopeSignature godoc).
//
// The returned slice preserves the input draft order; the
// orchestrator's Stage 2.2 sort already pinned it
// deterministically.
//
// BuildSamples ALWAYS returns a non-nil slice -- a clean
// repo or a partial parser run that emits zero drafts
// yields a length-0 (but non-nil) result so the downstream
// [LoadStore] nil-samples precondition treats "no findings"
// as a valid empty batch rather than a wiring bug.
func BuildSamples(
	repoCtx repocontext.RepoContext,
	drafts []recipes.MetricSampleDraft,
	bindings *scopebinding.Table,
	scopeIDs map[ScopeBindingKey]uuid.UUID,
) []rule_engine.Sample {
	out := make([]rule_engine.Sample, 0, len(drafts))
	for _, d := range drafts {
		key := ScopeBindingKey{Path: d.Scope.Path, LocalID: d.Scope.LocalID}
		scopeID, ok := scopeIDs[key]
		if !ok || scopeID == uuid.Nil {
			continue
		}
		var signature string
		if bindings != nil {
			b, found := bindings.Get(scopeID)
			if !found || b.Signature == "" {
				continue
			}
			signature = b.Signature
		} else {
			continue
		}
		sampleID := MintSampleID(repoCtx.RepoID, repoCtx.HeadSHA, scopeID, d.MetricKind, d.MetricVersion)
		out = append(out, rule_engine.Sample{
			Sample: dsl.Sample{
				SampleID:      sampleID,
				RepoID:        repoCtx.RepoID,
				SHA:           repoCtx.HeadSHA,
				ScopeID:       scopeID,
				ScopeKind:     string(d.Scope.Kind),
				MetricKind:    d.MetricKind,
				MetricVersion: d.MetricVersion,
				Value:         d.Value,
				HasValue:      true,
				Pack:          string(d.Pack),
				Source:        string(d.Source),
			},
			ScopeSignature: signature,
		})
	}
	return out
}

// MintSampleID derives a deterministic UUID-v5 for the
// `(repoID, headSHA, scopeID, metricKind, metricVersion)`
// tuple so re-runs over the same checkout produce byte-
// identical sample rows (tech-spec C11). The CLI is the
// first writer for these rows and there is no Ingestor in
// the loop, so the recipe-tuple identity is the
// authoritative natural key.
//
// The CLI-specific [SampleIDNamespacePrefix] keeps these
// ids in a separate namespace from any production-side
// Ingestor mint so a future shared store could not collide.
func MintSampleID(repoID uuid.UUID, headSHA string, scopeID uuid.UUID, metricKind string, metricVersion int) uuid.UUID {
	name := fmt.Sprintf("%s%s|%s|%s|%s|%d",
		SampleIDNamespacePrefix,
		repoID.String(),
		headSHA,
		scopeID.String(),
		metricKind,
		metricVersion,
	)
	return uuid.NewV5(uuid.NamespaceURL, name)
}

// ErrLoadStoreNilSamples is returned from [LoadStore] when
// the caller passes a nil samples slice. The CLI
// composition root maps this to exit code 70 BEFORE any
// engine invocation -- the upstream Stage 2.2 pipeline
// must always produce a (possibly empty) slice; a nil
// argument signals a wiring bug, not "no findings".
//
// The check is the precondition gate the workstream brief
// pins ahead of the void [rule_engine.InMemoryStore.InsertSamples]
// call: because InsertSamples has no error return, the
// pipeline cannot discover the wiring bug after the fact.
var ErrLoadStoreNilSamples = fmt.Errorf("orchestrator: LoadStore: samples slice is nil (wiring bug); cleanc exits 70 before engine invocation")

// loadStore is the brief-shape helper the workstream pins:
//
//	loadStore(bundle devpolicy.Bundle, samples []rule_engine.Sample) *rule_engine.InMemoryStore
//
// It seeds a fresh [rule_engine.InMemoryStore] with
// `architecture.md` Sec 3.4 steps 1 and 2 -- one
// [rule_engine.InMemoryStore.InsertPolicyVersion] call for
// `bundle.PolicyVersion` and one
// [rule_engine.InMemoryStore.InsertRule] per entry in
// `bundle.Rules`. It returns the store unconditionally
// (the underlying constructor and Insert* methods are void;
// they cannot fail).
//
// Step 3 (`store.InsertSamples(repoID, sha, samples)`)
// requires a [repocontext.RepoContext] for the
// `(repoID, sha)` coordinates and so cannot be performed
// inside this signature -- callers either invoke
// [LoadStore] (which wraps `loadStore` and adds step 3 with
// the nil-samples precondition guard) or invoke
// `store.InsertSamples` themselves. The `samples` argument
// is accepted here to honor the brief literally; the
// returned store does NOT yet contain those samples.
//
// A nil `samples` argument is accepted by this helper
// (callers that want the wiring-bug guard MUST go through
// [LoadStore] instead). An empty (zero-length) slice is
// also valid and reflects a clean repo with no metric
// drafts -- the policy and rules are still seeded so the
// engine can `RunBatch` and emit zero findings.
func loadStore(bundle devpolicy.Bundle, samples []rule_engine.Sample) *rule_engine.InMemoryStore {
	_ = samples // accepted for brief-contract parity; see godoc.

	store := rule_engine.NewInMemoryStore()

	// Sec 3.4 step 1: register the policy version.
	store.InsertPolicyVersion(bundle.PolicyVersion)

	// Sec 3.4 step 2: register every rule the bundle's
	// YAML loader emitted. One InsertRule call per entry
	// per the workstream brief.
	for _, r := range bundle.Rules {
		store.InsertRule(r)
	}

	return store
}

// LoadStore builds a [rule_engine.InMemoryStore] seeded
// with the dev-mode policy version and rule rows from
// [bundle] (architecture Sec 3.4 steps 1-2, via the
// brief-shape [loadStore] helper) plus the [samples] slice
// from [BuildSamples] (Sec 3.4 step 3).
//
// The store returned is ready to be passed into
// [rule_engine.New] as `Config.Store`; the CLI then calls
// `engine.RunBatch(ctx, repoCtx.RepoID, repoCtx.HeadSHA,
// bundle.PolicyVersion.PolicyVersionID)` and the genuine
// `(*Engine, error)` / `(RunResult, error)` surfaces from
// `rule_engine.New` (engine.go:133) and `engine.RunBatch`
// (engine.go:197) carry any downstream failure -- this
// function is the void-API precondition gate ONLY.
//
// `samples` MUST NOT be nil. A nil slice signals an
// upstream wiring bug ([BuildSamples] always returns a
// non-nil slice, including a length-0 slice for a clean
// repo) and triggers [ErrLoadStoreNilSamples]. An empty
// (but non-nil) slice is the canonical "no findings"
// signal: the policy + rules are still seeded so the
// engine can `RunBatch` without samples and emit zero
// findings.
func LoadStore(bundle devpolicy.Bundle, samples []rule_engine.Sample, repoCtx repocontext.RepoContext) (*rule_engine.InMemoryStore, error) {
	if samples == nil {
		return nil, ErrLoadStoreNilSamples
	}

	store := loadStore(bundle, samples)
	if store == nil {
		// Defensive: NewInMemoryStore is documented as
		// always returning a non-nil value, but the
		// workstream brief pins a nil-store guard ahead of
		// the void InsertSamples call so a future
		// regression here surfaces as exit 70 rather than
		// a nil-pointer panic deep in the engine.
		return nil, fmt.Errorf("orchestrator: LoadStore: rule_engine.NewInMemoryStore returned nil")
	}

	// Sec 3.4 step 3: seed the measurement mirror with the
	// CLI-synthesised samples. The canonical plural /
	// batched API at inmem_store.go:146-151 returns no
	// value -- the nil-samples guard above is the only
	// pre-flight check that can reject a wiring bug
	// without a panic.
	store.InsertSamples(repoCtx.RepoID, repoCtx.HeadSHA, samples)

	return store, nil
}
