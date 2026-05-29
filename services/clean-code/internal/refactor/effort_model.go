package refactor

// Stage 8.3 of the Refactor Planner: ML effort-model loader
// and version pinning. The Stage 8.2 [TaskPlanner] emits one
// [RefactorTask] per (hotspot, rule_id) pair with
// [RefactorTask.EffortHours] populated by the configured
// [EffortModel]. The model is selected at composition-root
// time from [config.Config.RefactorEffortSource] and the
// `CLEAN_CODE_ML_MODEL_URI` / `CLEAN_CODE_ML_MODEL_VERSION`
// operator pins.
//
// # Architecture references
//
//   - Architecture Sec 1.6 row 5: operator pin
//     `refactor-effort-source` -- canonical default is
//     "ML model from historical commits".
//   - Architecture Sec 5.5.3 + Sec 8.3: the loaded model's
//     semantic version MUST equal
//     `policy_version.refactor_weights.effort_model_version`
//     at every [TaskPlanner.Plan] call; reproducibility
//     traverses
//     `refactor_task -> refactor_plan -> hot_spot.policy_version_id
//     -> policy_version.refactor_weights.effort_model_version`
//     so the effort estimate is reconstructible at any future
//     audit point.
//   - Implementation-plan Stage 8.3 line 751: the model
//     version is NOT duplicated on `refactor_task` or
//     `refactor_plan`; the inheritance chain above is the
//     single source of truth.
//
// # Why Estimate returns (float64, error)
//
// Rubber-duck Stage 9.3 design review caught that a bare
// `float64` return cannot signal:
//
//   - unsupported [EffortSource] (typo in operator pin),
//   - missing model URI / version when the source is `ml`,
//   - model-version drift from the policy version's
//     `effort_model_version`,
//   - non-finite / negative estimates (NaN, ±Inf, < 0).
//
// Returning a typed error lets [TaskPlanner.PlanFromSnapshot]
// abort the WHOLE atomic plan + tasks write rather than
// landing a row with a silently-bogus `effort_hours`. The
// planner already aborts the batch on bad [TaskKind]; the
// effort path matches that contract.

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// EffortSource is the closed enum of refactor-effort source
// selectors. Mirrors the architecture Sec 1.6 row-5 pin
// vocabulary and the docker-compose shorthand.
type EffortSource string

const (
	// EffortSourceZero stamps `0.0` for every task -- the
	// Stage 8.2 explicit "unestimated" placeholder. Used by
	// in-memory test fixtures and by deployments that have
	// not yet trained an ML model.
	EffortSourceZero EffortSource = "zero"

	// EffortSourceHeuristic derives effort from the hotspot
	// score and task kind without any ML model. The estimate
	// is a deterministic function of the input rows. Useful
	// for staging environments and as a fallback when the ML
	// artefact is unavailable.
	EffortSourceHeuristic EffortSource = "heuristic"

	// EffortSourceML loads the ML model artefact named by
	// [config.EnvMLModelURI] and pins its version against
	// [config.EnvMLModelVersion]. The loaded version MUST
	// equal `policy_version.refactor_weights.effort_model_version`
	// at every [TaskPlanner.Plan] call (architecture Sec 5.5.3
	// + Sec 8.3).
	EffortSourceML EffortSource = "ml"
)

// canonicalArchitecturePinAlias is the long-form architecture
// Sec 1.6 row-5 default. The cmd binary accepts the short
// `ml` and the long-form interchangeably so the operator's
// canonical pin OR the compose shorthand both resolve to the
// same source.
const canonicalArchitecturePinAlias = "ML model from historical commits"

// ResolveEffortSource normalises an operator-supplied
// effort-source string (either the architecture canonical pin
// or the compose shorthand) to one of [EffortSourceZero],
// [EffortSourceHeuristic], or [EffortSourceML]. Returns
// [ErrUnknownEffortSource] for any unrecognised value.
//
// The empty string resolves to [EffortSourceZero] -- the
// Stage 8.2 placeholder, preserving the planner's existing
// "unestimated" semantics for scaffold deployments.
func ResolveEffortSource(raw string) (EffortSource, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return EffortSourceZero, nil
	}
	switch strings.ToLower(trimmed) {
	case string(EffortSourceZero), "0", "off", "none", "placeholder":
		return EffortSourceZero, nil
	case string(EffortSourceHeuristic), "heuristics":
		return EffortSourceHeuristic, nil
	case string(EffortSourceML),
		strings.ToLower(canonicalArchitecturePinAlias),
		"ml-model",
		"ml_model":
		return EffortSourceML, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownEffortSource, raw)
	}
}

// EffortModel estimates `refactor_task.effort_hours` for a
// single task at emit time. Implementations MUST be
// goroutine-safe (the planner does not serialise calls) and
// SHOULD be deterministic for the same (task, hs, snap)
// triple so two runs of the same `(repo_id, sha)` produce
// byte-identical row sets.
//
// Returns:
//
//   - estimate in hours (finite, non-negative).
//   - error wrapping [ErrUnknownEffortSource],
//     [ErrMLModelURIMissing], [ErrMLModelVersionMissing],
//     [ErrMLModelVersionMismatch], or
//     [ErrInvalidEffortEstimate] depending on the failure
//     mode. The planner ABORTS the batch on any non-nil
//     error -- partial emission would violate the atomic
//     plan + tasks write contract.
type EffortModel interface {
	Estimate(task RefactorTask, hs HotSpot, snap PolicySnapshot) (float64, error)
}

// EffortModelFunc adapts a plain function into [EffortModel].
// Mirrors `http.HandlerFunc` -- composition-root wiring that
// wants to inject a one-line stub does not need to declare a
// new type.
type EffortModelFunc func(task RefactorTask, hs HotSpot, snap PolicySnapshot) (float64, error)

// Estimate implements [EffortModel].
func (f EffortModelFunc) Estimate(task RefactorTask, hs HotSpot, snap PolicySnapshot) (float64, error) {
	if f == nil {
		return 0, ErrNilEffortFunc
	}
	return f(task, hs, snap)
}

// -----------------------------------------------------------------------------
// Concrete implementations
// -----------------------------------------------------------------------------

// ZeroEffortModel stamps `0.0` for every task. This is the
// Stage 8.2 placeholder semantics preserved verbatim.
// Distinct from "the planner is broken" -- the constant zero
// is meaningful (architecture Sec 5.5.3: `EffortHours == 0`
// is the "unestimated" wire value).
type ZeroEffortModel struct{}

// Estimate implements [EffortModel] -- always returns
// (0.0, nil).
func (ZeroEffortModel) Estimate(_ RefactorTask, _ HotSpot, _ PolicySnapshot) (float64, error) {
	return 0.0, nil
}

// HeuristicEffortModel derives an estimate from the hotspot
// score and the task kind, without any external model
// artefact. Useful in staging environments and as a fallback
// when the ML model is unavailable. The estimate is purely a
// function of the inputs; two runs at the same (task, hs)
// produce byte-identical output.
//
// Formula (deterministic, finite, non-negative):
//
//	base    := kindBaseHours[task.Kind]   // canonical-kind table
//	scaled  := base * (1 + clamp(hs.Score, 0, 10))
//	clamped := math.Min(scaled, MaxHeuristicHours)
//
// `MaxHeuristicHours` (40h = one nominal sprint week) caps
// the estimate so an outlier hotspot score does not produce
// an absurd "320-hour refactor" task.
type HeuristicEffortModel struct{}

// MaxHeuristicHours is the upper bound on
// [HeuristicEffortModel] output. Documented so callers and
// tests can rely on the value.
const MaxHeuristicHours = 40.0

// Estimate implements [EffortModel].
func (HeuristicEffortModel) Estimate(task RefactorTask, hs HotSpot, _ PolicySnapshot) (float64, error) {
	if err := ValidateTaskKind(task.Kind); err != nil {
		return 0, fmt.Errorf("HeuristicEffortModel.Estimate: %w", err)
	}
	base := kindBaseHours(task.Kind)
	score := hs.Score
	if math.IsNaN(score) || math.IsInf(score, 0) {
		score = 0
	}
	if score < 0 {
		score = 0
	}
	if score > 10 {
		score = 10
	}
	out := base * (1.0 + score)
	if out > MaxHeuristicHours {
		out = MaxHeuristicHours
	}
	if err := validateEstimate(out); err != nil {
		return 0, fmt.Errorf("HeuristicEffortModel.Estimate: %w", err)
	}
	return out, nil
}

// MLEffortModel is the v0 ML effort-model adapter. Stage 9.3
// scope explicitly extends Stage 9.3 to wire the operator's
// `CLEAN_CODE_ML_MODEL_URI` + `CLEAN_CODE_ML_MODEL_VERSION`
// envs into the planner; the v0 estimator is a stable,
// deterministic FNV-1a hash of the input row identifiers
// scaled into the [0, MaxMLHours] range. A real ONNX / TF
// inference adapter is a follow-up workstream (Stage 10.x);
// it plugs in by replacing this struct's [Estimate] body
// without changing the [EffortModel] interface or the
// composition-root wiring.
//
// The version pin guards the architecture Sec 8.3
// reproducibility invariant: the planner's [PolicySnapshot]
// carries `refactor_weights.effort_model_version` and a
// mismatch with the operator-pinned [ModelVersion] returns
// [ErrMLModelVersionMismatch], aborting the whole atomic
// plan + tasks write.
type MLEffortModel struct {
	// ModelURI is the operator-supplied URI of the model
	// artefact. Non-empty -- [NewMLEffortModel] rejects an
	// empty URI with [ErrMLModelURIMissing].
	ModelURI string

	// ModelVersion is the semantic version the loaded model
	// claims (matched against `policy_version.refactor_weights.effort_model_version`
	// at estimate time). Non-empty -- [NewMLEffortModel]
	// rejects an empty version with [ErrMLModelVersionMissing].
	ModelVersion string

	// ArtefactBytes is the size of the loaded artefact (the
	// file pointed at by a `file://` URI). Zero for non-file
	// URIs that fall through the loader's `noop` branch
	// (test fixtures). The size is folded into [Estimate]'s
	// hash so two MLEffortModel instances pinned to the same
	// version but loaded from different artefact bytes do not
	// silently collapse to the same estimate.
	ArtefactBytes int64

	// artefactDigest is the FNV-1a 64-bit hash of the loaded
	// artefact contents. Folded into [Estimate] so the
	// estimator's output reflects the loaded model bytes;
	// swapping the artefact in place between deploys changes
	// the estimate (a real reproducibility guarantee, not
	// just a version-string check).
	artefactDigest uint64
}

// MaxMLHours bounds the v0 ML estimator's output to the same
// scale as [HeuristicEffortModel] so the cap is uniform across
// effort-source choices. Documented so tests can rely on it.
const MaxMLHours = MaxHeuristicHours

// MLArtefactLoader resolves an `MLEffortModel.ModelURI` to
// the artefact's raw bytes. Pluggable so a future Stage 10.x
// can swap in an ONNX-runtime loader without changing
// [NewMLEffortModel] or any caller. The default
// [defaultMLArtefactLoader] handles `file://` URIs by opening
// the local filesystem path; everything else returns
// (nil, [ErrMLModelArtefactInvalid]).
type MLArtefactLoader func(uri string) ([]byte, error)

// NewMLEffortModel validates the URI + version pair and
// returns a ready-to-use estimator. Returns
// [ErrMLModelURIMissing] when uri is empty,
// [ErrMLModelVersionMissing] when version is empty,
// [ErrMLModelArtefactInvalid] when the URI points at a
// missing / empty / unsupported artefact.
func NewMLEffortModel(uri, version string) (*MLEffortModel, error) {
	return newMLEffortModelWithLoader(uri, version, defaultMLArtefactLoader)
}

// newMLEffortModelWithLoader is the testable variant of
// [NewMLEffortModel] that accepts a loader override. The
// production constructor always uses [defaultMLArtefactLoader].
func newMLEffortModelWithLoader(uri, version string, loader MLArtefactLoader) (*MLEffortModel, error) {
	trimmedURI := strings.TrimSpace(uri)
	trimmedVersion := strings.TrimSpace(version)
	if trimmedURI == "" {
		return nil, ErrMLModelURIMissing
	}
	if trimmedVersion == "" {
		return nil, ErrMLModelVersionMissing
	}
	if loader == nil {
		loader = defaultMLArtefactLoader
	}
	payload, err := loader(trimmedURI)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMLModelArtefactInvalid, err)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: artefact is empty at %q",
			ErrMLModelArtefactInvalid, trimmedURI)
	}
	digest := fnv.New64a()
	_, _ = digest.Write(payload)
	return &MLEffortModel{
		ModelURI:       trimmedURI,
		ModelVersion:   trimmedVersion,
		ArtefactBytes:  int64(len(payload)),
		artefactDigest: digest.Sum64(),
	}, nil
}

// defaultMLArtefactLoader resolves `file://` URIs to the
// referenced file's contents. The loader is intentionally
// minimal: it does NOT attempt to interpret ONNX / TF model
// bytes; it only proves the artefact exists, is non-empty,
// and is reachable from the running process. A future
// Stage 10.x can replace this with a full ONNX-runtime
// loader without changing the [MLEffortModel] interface.
//
// Returns ([]byte, nil) on success; (nil, error) for:
//
//   - URIs whose scheme is not `file`.
//   - file:// URIs whose path is unreadable / missing.
//   - file:// URIs that resolve to a directory.
//   - file:// URIs whose target file is empty.
//
// The size cap (16 MiB) prevents a misconfigured URI from
// reading a multi-GiB file into the planner's address space.
const mlArtefactMaxBytes = 16 * 1024 * 1024

func defaultMLArtefactLoader(uri string) ([]byte, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("parse uri %q: %w", uri, err)
	}
	switch parsed.Scheme {
	case "file":
		return loadFileURIArtefact(parsed)
	case "":
		return nil, fmt.Errorf(
			"uri %q has no scheme; supported schemes: file://",
			uri)
	default:
		return nil, fmt.Errorf(
			"uri scheme %q unsupported; supported: file:// (Stage 10.x will extend to https/s3)",
			parsed.Scheme)
	}
}

// loadFileURIArtefact resolves a `file://` URL to local-disk
// bytes. Per RFC 8089 the host part is optional and (when
// present) MUST be `localhost`; we accept both
// `file:///path/to/x` and `file://localhost/path/to/x` as
// equivalent. Anything else (e.g. `file://example.com/x`) is
// rejected because a network fetch is not what the operator
// asked for.
func loadFileURIArtefact(parsed *url.URL) ([]byte, error) {
	if parsed.Host != "" && parsed.Host != "localhost" {
		return nil, fmt.Errorf(
			"file:// URI with remote host %q is unsupported (Stage 10.x: use https://)",
			parsed.Host)
	}
	path := parsed.Path
	if path == "" {
		return nil, fmt.Errorf("file:// URI has empty path: %q", parsed.String())
	}
	// On Windows, file:///C:/x.onnx parses with Path=/C:/x.onnx.
	// The leading slash before the drive letter is an artefact
	// of the URI spec; strip it so os.Stat resolves correctly.
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	clean := filepath.FromSlash(path)
	info, err := os.Stat(clean)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", clean, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("artefact path %q is a directory", clean)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("artefact file %q is empty", clean)
	}
	if info.Size() > mlArtefactMaxBytes {
		return nil, fmt.Errorf(
			"artefact file %q is %d bytes; max accepted is %d bytes",
			clean, info.Size(), mlArtefactMaxBytes)
	}
	payload, err := os.ReadFile(clean)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", clean, err)
	}
	return payload, nil
}

// Estimate implements [EffortModel]. Returns
// [ErrMLModelVersionMismatch] when
// `snap.Weights.EffortModelVersion` is non-empty and does not
// match the loaded [ModelVersion] -- the architecture Sec 8.3
// reproducibility invariant. An empty policy-side version is
// permitted (some test fixtures pre-date the field's
// introduction); production policies always carry the pin and
// the steward refuses to publish without it.
func (m *MLEffortModel) Estimate(task RefactorTask, hs HotSpot, snap PolicySnapshot) (float64, error) {
	if err := ValidateTaskKind(task.Kind); err != nil {
		return 0, fmt.Errorf("MLEffortModel.Estimate: %w", err)
	}
	policyVersion := strings.TrimSpace(snap.Weights.EffortModelVersion)
	if policyVersion != "" && policyVersion != m.ModelVersion {
		return 0, fmt.Errorf(
			"%w: model=%q policy=%q (architecture Sec 8.3: "+
				"policy_version.refactor_weights.effort_model_version "+
				"MUST match the loaded model version)",
			ErrMLModelVersionMismatch, m.ModelVersion, policyVersion)
	}
	// v0 deterministic estimator: FNV-1a hash of the
	// (model_version, artefact_digest, scope_id, rule_id,
	// kind, score) reduced to [0, MaxMLHours]. Stable across
	// runs -- reproducibility under audit is preserved.
	// Folding `artefactDigest` makes the estimate reflect
	// the loaded artefact bytes, not just the version pin:
	// swapping the file in place between deploys changes
	// the output even when the version string is unchanged.
	h := fnv.New64a()
	_, _ = h.Write([]byte(m.ModelVersion))
	_, _ = h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "%016x", m.artefactDigest)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(task.ScopeID.Bytes())
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(task.RuleID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(task.Kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(fmt.Sprintf("%.6f", clampScore(hs.Score))))
	frac := float64(h.Sum64()%10001) / 10000.0
	base := kindBaseHours(task.Kind)
	out := base + (MaxMLHours-base)*frac
	if out < 0 {
		out = 0
	}
	if out > MaxMLHours {
		out = MaxMLHours
	}
	if err := validateEstimate(out); err != nil {
		return 0, fmt.Errorf("MLEffortModel.Estimate: %w", err)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Factory
// -----------------------------------------------------------------------------

// EffortModelConfig is the narrow surface
// [NewEffortModelFromConfig] reads. Keeping the dependency on
// a small struct (rather than the full `config.Config`) lets
// the refactor package stay free of an import on
// `internal/config` while still consuming the operator-pinned
// values verbatim. The cmd binary populates this from the
// loaded [config.Config] at composition-root time.
type EffortModelConfig struct {
	// Source is the raw operator pin string -- either the
	// canonical "ML model from historical commits" or the
	// compose shorthand ("zero" | "heuristic" | "ml"). The
	// constructor normalises via [ResolveEffortSource].
	Source string

	// MLModelURI populates [MLEffortModel.ModelURI]. Required
	// when [Source] resolves to [EffortSourceML].
	MLModelURI string

	// MLModelVersion populates [MLEffortModel.ModelVersion].
	// Required when [Source] resolves to [EffortSourceML].
	MLModelVersion string
}

// NewEffortModelFromConfig constructs the [EffortModel]
// implementation selected by the supplied config. Returns
// [ErrUnknownEffortSource] when the source pin is unrecognised,
// [ErrMLModelURIMissing] / [ErrMLModelVersionMissing] when the
// ML branch is selected without the matching pins.
//
// Composition-root example (cmd/clean-code-refactor-planner):
//
//	em, err := refactor.NewEffortModelFromConfig(refactor.EffortModelConfig{
//	    Source:         cfg.RefactorEffortSource,
//	    MLModelURI:     cfg.MLModelURI,
//	    MLModelVersion: cfg.MLModelVersion,
//	})
//	if err != nil {
//	    return fmt.Errorf("effort model: %w", err)
//	}
//	tp, err := refactor.NewTaskPlanner(..., refactor.WithEffortModel(em))
func NewEffortModelFromConfig(cfg EffortModelConfig) (EffortModel, error) {
	src, err := ResolveEffortSource(cfg.Source)
	if err != nil {
		return nil, err
	}
	switch src {
	case EffortSourceZero:
		return ZeroEffortModel{}, nil
	case EffortSourceHeuristic:
		return HeuristicEffortModel{}, nil
	case EffortSourceML:
		return NewMLEffortModel(cfg.MLModelURI, cfg.MLModelVersion)
	default:
		// Defensive: ResolveEffortSource is exhaustive over
		// the closed enum, so this branch is unreachable in
		// practice. Surface the typed sentinel rather than a
		// panic so the cmd binary can wrap it in its startup
		// error.
		return nil, fmt.Errorf("%w: resolved=%q", ErrUnknownEffortSource, src)
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// kindBaseHours returns a per-kind baseline estimate the
// heuristic and ML models share. The five-kind table mirrors
// [CanonicalTaskKinds]; unknown kinds yield 1.0 hour as a
// defensive default, but the caller is expected to have run
// [ValidateTaskKind] FIRST so this branch is unreachable in
// production paths.
func kindBaseHours(k TaskKind) float64 {
	switch k {
	case TaskKindSplitClass:
		return 8.0
	case TaskKindExtractMethod:
		return 2.0
	case TaskKindInvertDependency:
		return 4.0
	case TaskKindBreakCycle:
		return 6.0
	case TaskKindConsolidateDuplication:
		return 3.0
	default:
		return 1.0
	}
}

// clampScore normalises an arbitrary hot-spot score into a
// finite [0, 10] window so the heuristic and ML estimators
// behave identically for outlier scores (NaN, ±Inf, < 0,
// > 10).
func clampScore(s float64) float64 {
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return 0
	}
	if s < 0 {
		return 0
	}
	if s > 10 {
		return 10
	}
	return s
}

// validateEstimate rejects NaN, ±Inf, and negative outputs.
// The estimator implementations clamp before calling this so
// it is a belt-and-braces check; a future implementation that
// forgets to clamp will surface [ErrInvalidEffortEstimate]
// rather than silently writing a bad row.
func validateEstimate(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%w: non-finite (%v)", ErrInvalidEffortEstimate, v)
	}
	if v < 0 {
		return fmt.Errorf("%w: negative (%v)", ErrInvalidEffortEstimate, v)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

var (
	// ErrUnknownEffortSource is returned by
	// [ResolveEffortSource] and [NewEffortModelFromConfig]
	// when the operator-supplied pin is not one of the
	// recognised values (typo, future-spec value, or stale
	// fixture). The cmd binary fails fast on this error so
	// the deployment cannot silently run with a default
	// model the operator did not ask for.
	ErrUnknownEffortSource = errors.New(
		"refactor: unknown effort source (allowed: zero|heuristic|ml " +
			"or the canonical 'ML model from historical commits')")

	// ErrMLModelURIMissing signals an [EffortSourceML]
	// selection without [config.EnvMLModelURI]. The cmd
	// binary refuses to start because the operator's pin
	// promised an ML estimate but did not supply the
	// artefact location.
	ErrMLModelURIMissing = errors.New(
		"refactor: effort source is ml but CLEAN_CODE_ML_MODEL_URI is empty")

	// ErrMLModelVersionMissing signals an [EffortSourceML]
	// selection without [config.EnvMLModelVersion].
	ErrMLModelVersionMissing = errors.New(
		"refactor: effort source is ml but CLEAN_CODE_ML_MODEL_VERSION is empty")

	// ErrMLModelVersionMismatch signals a drift between the
	// loaded ML model's version and the active policy's
	// `refactor_weights.effort_model_version`. The planner
	// aborts the whole atomic plan + tasks write so the
	// architecture Sec 8.3 reproducibility chain stays
	// intact -- no `refactor_task` row carries an estimate
	// that was produced by a model the policy did not pin.
	ErrMLModelVersionMismatch = errors.New(
		"refactor: ML model version does not match " +
			"policy_version.refactor_weights.effort_model_version")

	// ErrMLModelArtefactInvalid signals that the
	// [MLArtefactLoader] could not resolve the operator's
	// URI to a usable artefact. The cmd binary fails fast on
	// this error: a `file://` URI pointing at a missing /
	// empty / oversized file MUST NOT silently degrade to a
	// no-op estimator. The error message names the exact
	// path / scheme the loader rejected so an SRE can fix
	// the deploy without code-spelunking.
	ErrMLModelArtefactInvalid = errors.New(
		"refactor: ML model artefact is invalid (missing, empty, " +
			"unreadable, oversized, or unsupported URI scheme)")

	// ErrInvalidEffortEstimate signals an estimator returned
	// NaN, ±Inf, or a negative value. Aborts the batch so a
	// `refactor_task.effort_hours` row never lands with a
	// non-finite value -- the column is `DOUBLE NOT NULL`
	// and downstream consumers (Insights aggregates,
	// management read verbs) assume real numbers.
	ErrInvalidEffortEstimate = errors.New(
		"refactor: effort estimate is not a finite, non-negative number")

	// ErrNilEffortModel signals a composition-root wiring
	// bug at [NewTaskPlanner] time: an option passed nil
	// through [WithEffortModel].
	ErrNilEffortModel = errors.New(
		"refactor: WithEffortModel was passed nil")

	// ErrNilEffortFunc is returned by [EffortModelFunc.Estimate]
	// when invoked on a nil func value. Caught at call time
	// rather than at construction so callers who deliberately
	// inject a zero-value adapter (e.g. test wiring that
	// later replaces it) get a clear error.
	ErrNilEffortFunc = errors.New(
		"refactor: EffortModelFunc invoked while nil")
)
