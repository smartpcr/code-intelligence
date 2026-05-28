package refactor

// Stage 8.3 effort-model loader + version pinning per
// architecture Sec 1.6 (operator pin `refactor-effort-source`,
// default `ML model from historical commits`) and Sec 5.3.3
// (`PolicyVersion.refactor_weights.effort_model_version`). The
// file ships:
//
//   - [EffortModel] -- the in-memory model artefact (version +
//     deterministic per-kind / per-Score coefficients) loaded
//     from a JSON blob on disk.
//   - [EffortEstimator] -- the narrow interface
//     [TaskPlanner] consumes via the new
//     [WithEffortEstimator] option. Implementations are
//     stateless and MUST be deterministic per architecture G6
//     (same model + same hotspot + same task = same
//     `effort_hours`).
//   - [LoadModelFromFile] / [LoadFromConfig] -- the two
//     loaders the composition root wires. `LoadFromConfig`
//     applies the architecture Sec 1.6 interlock: when
//     `refactor-effort-source` requires a model AND the URI
//     is empty, startup is refused with [ErrEffortModelURIRequired].
//   - URI parsing -- accepts a bare local path
//     (`/abs/path/to/model.json`, `C:\path\to\model.json`) OR
//     a `file://` URI (`file:///abs/path` on POSIX,
//     `file:///C:/path` on Windows). Other schemes (`http://`,
//     `s3://`, ...) return [ErrEffortModelUnsupportedScheme]
//     so a typo in the operator pin fails fast instead of
//     silently 404-ing at runtime.
//
// # Version-pinning invariant (architecture Sec 5.3.3 +
//   workstream brief)
//
// The Stage 8.3 architecture deliberately AVOIDS duplicating
// `effort_model_version` onto `refactor_plan` or
// `refactor_task`. Recovery of the model version that
// produced a given `refactor_task.effort_hours` goes
//
//	refactor_task -> refactor_plan
//	            -> hot_spot (via refactor_plan.hotspot_ids[0])
//	            -> policy_version (via hot_spot.policy_version_id)
//	            -> refactor_weights.effort_model_version
//
// [EffortModel.Estimate] enforces the head of that chain by
// REFUSING to score a task whose
// `snap.Weights.EffortModelVersion` does not equal
// `model.Version`. A mismatch indicates the operator either
// (a) republished a policy without retraining the loaded
// model, or (b) loaded a stale model artefact -- in both
// cases producing an estimate would silently violate the
// "model version pinned by the active policy" architecture
// invariant. The wrapped error is
// [ErrEffortModelVersionMismatch] so the
// [TaskPlanner.PlanFromSnapshot] caller can abort the whole
// batch (rubber-duck design review #2: silent fallback to
// `0.0` would hide the misconfiguration in production).
//
// # Deterministic functional form
//
// The architecture does not pin a single canonical model
// shape; the operator pin's natural-language value is "ML
// model from historical commits", which is a class of model
// rather than a specific algorithm. v0 ships the simplest
// deterministic class: a per-`TaskKind` base-hours lookup
// plus a linear score-coefficient term + intercept, all
// clamped to >= 0 (negative effort is meaningless):
//
//	hours = max(0, base[kind] + score_coef * hot_spot.score + intercept)
//
// The inputs are EXACTLY the fields persisted on the canonical
// `clean_code.hot_spot` row + `clean_code.refactor_task` row:
// `hs.Score` and `task.Kind`. Per architecture Sec 5.5.1, the
// `hot_spot` row does NOT persist the per-input z-score
// breakdown -- only the composite `score`. A model that
// required breakdown fields could not be re-applied to a
// historical hot_spot row (rubber-duck design review #1).
//
// The artefact format is a strict-schema JSON blob; missing
// fields, NaN/Inf coefficients, empty version, or missing
// per-kind base entries are rejected by
// [validateLoadedModel].

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
)

// -----------------------------------------------------------------------------
// Operator pin closed set (architecture Sec 1.6 row 5)
// -----------------------------------------------------------------------------

// RefactorEffortSourceMLModel is the canonical default value
// of the operator pin `refactor-effort-source` per
// architecture Sec 1.6 row 5. The string matches
// [config.DefaultRefactorEffortSource] verbatim; pinning the
// constant here (rather than re-importing the config default)
// makes a `grep -nF "ML model from historical commits"` over
// the planner tree land on both call sites.
const RefactorEffortSourceMLModel = "ML model from historical commits"

// RefactorEffortSourceNone is the explicit opt-out value of
// the operator pin `refactor-effort-source`. When set,
// [LoadFromConfig] returns `(nil, nil)` and the composition
// root MUST NOT wire an [EffortEstimator]; the planner then
// emits the Stage 8.2 `effort_hours = 0.0` placeholder for
// every task.
//
// "none" is NOT enumerated in architecture Sec 1.6 verbatim;
// it is the implementation's "no model loaded" escape hatch
// for staging environments that genuinely cannot supply a
// trained artefact (or for an integration test that needs
// the Stage 8.2 byte-identical path). Production deployments
// MUST set the pin to [RefactorEffortSourceMLModel] AND
// configure the URI; the loader refuses any other value.
const RefactorEffortSourceNone = "none"

// requiresModel reports whether the operator pin value
// requires a model artefact to be configured. The closed set
// is `{RefactorEffortSourceMLModel, RefactorEffortSourceNone}`;
// any other value is rejected by [LoadFromConfig] with
// [ErrEffortModelSourceUnknown].
func requiresModel(source string) (bool, error) {
	switch source {
	case RefactorEffortSourceMLModel:
		return true, nil
	case RefactorEffortSourceNone:
		return false, nil
	default:
		return false, fmt.Errorf(
			"%w: %q (allowed: %q | %q)",
			ErrEffortModelSourceUnknown,
			source,
			RefactorEffortSourceMLModel,
			RefactorEffortSourceNone,
		)
	}
}

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

var (
	// ErrEffortModelURIRequired is returned by
	// [LoadFromConfig] when the operator pin
	// `refactor-effort-source` requires a model artefact
	// AND the URI env var
	// [config.EnvRefactorEffortModelURI] is empty. The
	// error message names BOTH env vars so the operator
	// can patch the misconfiguration without grepping.
	ErrEffortModelURIRequired = errors.New(
		"refactor: effort-model URI is required but not configured " +
			"(set CLEAN_CODE_REFACTOR_EFFORT_MODEL_URI or set " +
			"CLEAN_CODE_REFACTOR_EFFORT_SOURCE=none for the explicit opt-out)")

	// ErrEffortModelSourceUnknown is returned by
	// [LoadFromConfig] when `RefactorEffortSource` is not
	// in the closed set
	// `{RefactorEffortSourceMLModel, RefactorEffortSourceNone}`.
	// Distinct from [ErrEffortModelURIRequired] so the
	// startup log makes the cause obvious.
	ErrEffortModelSourceUnknown = errors.New(
		"refactor: CLEAN_CODE_REFACTOR_EFFORT_SOURCE value is not recognised")

	// ErrEffortModelUnsupportedScheme is returned by
	// [resolveModelPath] when the operator URI carries a
	// scheme other than the supported `file://` (or no
	// scheme = local path). v0 supports only local-disk
	// artefacts; remote URIs are out of scope.
	ErrEffortModelUnsupportedScheme = errors.New(
		"refactor: effort-model URI scheme is not supported in v0 (use file:// or a bare path)")

	// ErrEffortModelMalformed wraps a JSON decode failure
	// or a strict-schema validation failure on the artefact.
	// Carries the offending field in the wrap so the
	// operator can patch the artefact without spelunking.
	ErrEffortModelMalformed = errors.New(
		"refactor: effort-model artefact is malformed")

	// ErrEffortModelVersionEmpty is returned by
	// [validateLoadedModel] when the artefact's `version`
	// field is empty or whitespace-only. The version is the
	// LOAD-bearing field for the architecture Sec 5.3.3
	// pinning invariant; an empty version is unusable.
	ErrEffortModelVersionEmpty = errors.New(
		"refactor: effort-model artefact carries an empty version string")

	// ErrEffortModelVersionMismatch is returned by
	// [EffortModel.Estimate] when the snapshot's
	// `Weights.EffortModelVersion` does not equal the
	// loaded model's `Version`. The wrapping error message
	// names both versions so the operator can diagnose
	// which side drifted.
	ErrEffortModelVersionMismatch = errors.New(
		"refactor: effort-model version does not match policy_version.refactor_weights.effort_model_version " +
			"(the loaded model artefact and the active policy disagree on the pinned version; " +
			"re-publish the policy with the model's version or reload the matching artefact)")

	// ErrEffortModelMissingKindBase is returned by
	// [validateLoadedModel] when the artefact omits a
	// base-hours entry for one of the canonical
	// [CanonicalTaskKinds]. Every kind MUST have an entry;
	// the planner refuses to fall back silently because a
	// missing kind would produce a zero estimate that is
	// indistinguishable from the Stage 8.2 placeholder.
	ErrEffortModelMissingKindBase = errors.New(
		"refactor: effort-model artefact omits a base-hours entry for a canonical task.kind")

	// ErrEffortModelNonFiniteCoefficient is returned by
	// [validateLoadedModel] when any coefficient
	// (`base_hours[*]`, `score_coef`, `intercept`) is NaN
	// or +/-Inf. Non-finite arithmetic would propagate to
	// `refactor_task.effort_hours` and crash any downstream
	// consumer that assumes the column is a finite double.
	ErrEffortModelNonFiniteCoefficient = errors.New(
		"refactor: effort-model artefact carries a non-finite coefficient (NaN or Inf)")
)

// -----------------------------------------------------------------------------
// EffortModel + EffortEstimator
// -----------------------------------------------------------------------------

// EffortEstimator is the narrow interface the [TaskPlanner]
// consumes via [WithEffortEstimator]. The default planner
// behaviour (no estimator wired) emits the Stage 8.2
// `effort_hours = 0.0` placeholder; wiring an estimator
// swaps the zero for a real estimate.
//
// Implementations MUST be deterministic per architecture G6:
// the same `(task, hs, snap)` triple MUST produce the same
// float bit-pattern across calls AND across replays. The
// production implementation [EffortModel.Estimate] satisfies
// this because the formula is a pure function of
// `task.Kind`, `hs.Score`, and the loaded model coefficients
// -- no clock, no RNG, no IO.
//
// On any error (version mismatch, unknown kind, ...) the
// [TaskPlanner] aborts the whole batch -- no plan row, no
// task row lands. Silent fallback to `0.0` would hide
// production model/schema bugs (rubber-duck design review
// finding #2).
type EffortEstimator interface {
	Estimate(task RefactorTask, hs HotSpot, snap PolicySnapshot) (float64, error)
}

// EffortModel is the v0 deterministic effort-estimation
// artefact. The shape is intentionally simple: a per-`TaskKind`
// base-hours lookup combined with a linear `score_coef *
// hs.Score + intercept` term and clamped at zero.
//
// Persistence: the JSON-on-disk artefact has the same field
// names + JSON tags below. The artefact's `version` is the
// load-bearing pin per architecture Sec 5.3.3 -- it MUST
// match `policy_version.refactor_weights.effort_model_version`
// for the model to be applied to a task scored under that
// policy.
//
// Concurrency: `*EffortModel` is read-only after
// construction; safe to call [EffortModel.Estimate] from
// multiple goroutines.
type EffortModel struct {
	// Version is the load-bearing pin per architecture
	// Sec 5.3.3. Compared verbatim against
	// `PolicySnapshot.Weights.EffortModelVersion` inside
	// [EffortModel.Estimate]; a mismatch yields
	// [ErrEffortModelVersionMismatch].
	Version string `json:"version"`

	// KindBaseHours maps every canonical [TaskKind] (per
	// [CanonicalTaskKinds]) to its training-set mean
	// effort in hours. Every canonical kind MUST have an
	// entry; missing entries are rejected by
	// [validateLoadedModel] with
	// [ErrEffortModelMissingKindBase]. Non-canonical kinds
	// in the map are ignored (they cannot be emitted by
	// the planner because [ValidateTaskKind] rejects them
	// upstream).
	KindBaseHours map[TaskKind]float64 `json:"kind_base_hours"`

	// ScoreCoef is the linear coefficient applied to the
	// hot_spot's composite score. Positive values mean
	// "higher-scoring hot_spots take more effort". May be
	// zero (the model ignores the score and uses only the
	// per-kind base). Must be finite.
	ScoreCoef float64 `json:"score_coef"`

	// Intercept is the additive constant applied to every
	// estimate. May be negative (subtractive), but the
	// final estimate is clamped at >= 0 inside
	// [EffortModel.Estimate]. Must be finite.
	Intercept float64 `json:"intercept"`
}

// Estimate implements [EffortEstimator] per the formula
//
//	hours = max(0, KindBaseHours[task.Kind]
//	             + ScoreCoef * hs.Score
//	             + Intercept)
//
// Returns [ErrEffortModelVersionMismatch] when
// `snap.Weights.EffortModelVersion != m.Version`; returns
// [ErrEffortModelMissingKindBase] when `task.Kind` has no
// entry in `KindBaseHours` (defensive belt-and-braces
// against a custom rule-mapper that bypasses
// [ValidateTaskKind]); returns
// [ErrEffortModelNonFiniteCoefficient] when the computed
// estimate is non-finite (defensive guard against an input
// `hs.Score` of +/-Inf).
//
// The estimate is clamped at 0 (no negative effort).
func (m *EffortModel) Estimate(task RefactorTask, hs HotSpot, snap PolicySnapshot) (float64, error) {
	if m == nil {
		return 0, errors.New("refactor: EffortModel.Estimate called on nil receiver")
	}
	if snap.Weights.EffortModelVersion != m.Version {
		return 0, fmt.Errorf(
			"%w: model.Version=%q, snap.Weights.EffortModelVersion=%q",
			ErrEffortModelVersionMismatch,
			m.Version,
			snap.Weights.EffortModelVersion,
		)
	}
	base, ok := m.KindBaseHours[task.Kind]
	if !ok {
		return 0, fmt.Errorf(
			"%w: kind=%q (canonical set: %v)",
			ErrEffortModelMissingKindBase, task.Kind, CanonicalTaskKinds)
	}
	estimate := base + m.ScoreCoef*hs.Score + m.Intercept
	if math.IsNaN(estimate) || math.IsInf(estimate, 0) {
		return 0, fmt.Errorf(
			"%w: estimate=%v from base=%v score=%v score_coef=%v intercept=%v",
			ErrEffortModelNonFiniteCoefficient, estimate,
			base, hs.Score, m.ScoreCoef, m.Intercept)
	}
	if estimate < 0 {
		return 0, nil
	}
	return estimate, nil
}

// -----------------------------------------------------------------------------
// Loader -- LoadFromConfig + LoadModelFromFile
// -----------------------------------------------------------------------------

// LoadFromConfig applies the architecture Sec 1.6 +
// workstream-brief interlock:
//
//   - When `cfg.RefactorEffortSource == RefactorEffortSourceNone`,
//     returns `(nil, nil)` -- the composition root must NOT
//     wire an estimator, and the planner emits the Stage 8.2
//     `effort_hours = 0.0` placeholder. The URI value is
//     ignored.
//
//   - When `cfg.RefactorEffortSource == RefactorEffortSourceMLModel`
//     (the architecture-default value) AND
//     `cfg.RefactorEffortModelURI` is empty, returns
//     [ErrEffortModelURIRequired]. The composition root
//     SHOULD log the wrap and exit non-zero.
//
//   - When the URI is set, [LoadModelFromFile] is invoked on
//     the resolved path. Returns whatever the loader returns.
//
//   - Any other `cfg.RefactorEffortSource` value yields
//     [ErrEffortModelSourceUnknown].
//
// Production composition root (see
// `cmd/clean-code-refactor-planner/main.go`):
//
//	model, err := refactor.LoadFromConfig(cfg)
//	if err != nil { os.Exit(1) }    // missing model = fail-fast
//	// model == nil when source = "none"; wire estimator only when non-nil
//	opts := []refactor.TaskOption{}
//	if model != nil { opts = append(opts, refactor.WithEffortEstimator(model)) }
//	tp, _ := refactor.NewTaskPlanner(..., opts...)
func LoadFromConfig(cfg config.Config) (*EffortModel, error) {
	source := strings.TrimSpace(cfg.RefactorEffortSource)
	required, err := requiresModel(source)
	if err != nil {
		return nil, err
	}
	uri := strings.TrimSpace(cfg.RefactorEffortModelURI)
	if !required {
		// Pin = "none" -- ignore URI, return nil model.
		// Production deploys should not hit this path.
		return nil, nil
	}
	if uri == "" {
		return nil, fmt.Errorf(
			"%w (CLEAN_CODE_REFACTOR_EFFORT_SOURCE=%q)",
			ErrEffortModelURIRequired, source)
	}
	path, err := resolveModelPath(uri)
	if err != nil {
		return nil, fmt.Errorf("CLEAN_CODE_REFACTOR_EFFORT_MODEL_URI=%q: %w", uri, err)
	}
	return LoadModelFromFile(path)
}

// LoadModelFromFile reads + decodes + strict-validates the
// JSON artefact at `path`. The file MUST exist; missing-file
// returns a wrapped `os.ErrNotExist`. The JSON MUST decode
// cleanly into [EffortModel]; failures return
// [ErrEffortModelMalformed].
//
// The loader is the canonical entry-point for tests; it
// accepts a pre-resolved local path so tests can write a
// fixture artefact to `t.TempDir()` without going through
// the URI parser.
func LoadModelFromFile(path string) (*EffortModel, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("refactor: read effort-model artefact %q: %w", path, err)
	}
	var m EffortModel
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v (path=%q)",
			ErrEffortModelMalformed, err, path)
	}
	if err := validateLoadedModel(&m); err != nil {
		return nil, fmt.Errorf("%w (path=%q)", err, path)
	}
	return &m, nil
}

// validateLoadedModel applies the strict-schema invariants
// the artefact MUST satisfy:
//
//  1. `Version` is non-empty after whitespace trim.
//  2. Every canonical [TaskKind] has an entry in
//     `KindBaseHours`.
//  3. Every entry value in `KindBaseHours` is finite (no
//     NaN, no Inf) and >= 0 (negative effort is meaningless).
//  4. `ScoreCoef` and `Intercept` are finite.
//
// Returns the first violation as a wrapped sentinel; the
// caller wraps further with the artefact path.
func validateLoadedModel(m *EffortModel) error {
	if strings.TrimSpace(m.Version) == "" {
		return ErrEffortModelVersionEmpty
	}
	// Re-canonicalise so the in-memory value matches the
	// JSON literal exactly (trim leading/trailing whitespace
	// the operator may have typo'd).
	m.Version = strings.TrimSpace(m.Version)
	if m.KindBaseHours == nil {
		return fmt.Errorf("%w: kind_base_hours is missing", ErrEffortModelMalformed)
	}
	// Sort the canonical kinds slice into a stable order for
	// the error message so a missing-kind error is
	// reproducible across runs.
	canonical := make([]string, 0, len(CanonicalTaskKinds))
	for _, k := range CanonicalTaskKinds {
		canonical = append(canonical, string(k))
	}
	sort.Strings(canonical)
	for _, kStr := range canonical {
		k := TaskKind(kStr)
		v, ok := m.KindBaseHours[k]
		if !ok {
			return fmt.Errorf("%w: kind=%q (required: %v)",
				ErrEffortModelMissingKindBase, k, canonical)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: kind_base_hours[%q]=%v",
				ErrEffortModelNonFiniteCoefficient, k, v)
		}
		if v < 0 {
			return fmt.Errorf("%w: kind_base_hours[%q]=%v (must be >= 0)",
				ErrEffortModelMalformed, k, v)
		}
	}
	if math.IsNaN(m.ScoreCoef) || math.IsInf(m.ScoreCoef, 0) {
		return fmt.Errorf("%w: score_coef=%v",
			ErrEffortModelNonFiniteCoefficient, m.ScoreCoef)
	}
	if math.IsNaN(m.Intercept) || math.IsInf(m.Intercept, 0) {
		return fmt.Errorf("%w: intercept=%v",
			ErrEffortModelNonFiniteCoefficient, m.Intercept)
	}
	return nil
}

// -----------------------------------------------------------------------------
// URI / path resolution
// -----------------------------------------------------------------------------

// resolveModelPath translates the operator URI into a local
// filesystem path the loader can `os.ReadFile`. Accepted
// forms:
//
//   - A bare local path: `/abs/path/to/model.json` (POSIX),
//     `C:\path\to\model.json` (Windows). Returned verbatim.
//   - A `file://` URI: `file:///abs/path` on POSIX,
//     `file:///C:/path` on Windows. Decoded via
//     [url.Parse]; the scheme is stripped; the resulting
//     local path is returned.
//
// Other schemes (`http://`, `https://`, `s3://`, ...) return
// [ErrEffortModelUnsupportedScheme]. v0 supports only
// local-disk artefacts; a remote-fetch path is intentionally
// out of scope (no third-party dependencies). A future
// stage MAY add HTTP and S3 schemes; the closed-set design
// keeps the v0 surface small.
//
// Windows-vs-POSIX path handling:
//
//   - `file:///C:/path/to/file.json` on Windows: [url.Parse]
//     yields `Path = "/C:/path/to/file.json"`. We strip the
//     leading slash so `os.ReadFile` accepts the canonical
//     `C:/path/to/file.json` form (`C:` is a drive letter,
//     not a UNIX root).
//   - `file:///abs/path` on POSIX: [url.Parse] yields
//     `Path = "/abs/path"`. We return it verbatim.
//   - `file://host/path` (UNC on Windows) is intentionally
//     NOT supported in v0; the operator should use a bare
//     UNC path (`\\server\share\path`) without the scheme.
func resolveModelPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrEffortModelURIRequired
	}
	// Quick rejection of multi-scheme URIs (`s3://...`,
	// `http://...`). We accept the scheme as `file` only.
	// A bare local path is not a URI; [url.Parse] would
	// still succeed on `C:\path` (treating `c` as a scheme,
	// which is wrong), so handle the Windows drive-letter
	// case BEFORE invoking [url.Parse].
	if looksLikeBarePath(raw) {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: parse: %v", ErrEffortModelMalformed, err)
	}
	if u.Scheme == "" {
		return raw, nil
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("%w: scheme=%q", ErrEffortModelUnsupportedScheme, u.Scheme)
	}
	// `u.Path` is the URL-decoded path component.
	path := u.Path
	if path == "" {
		// `file://host` with no path is meaningless for v0.
		return "", fmt.Errorf("%w: file:// URI carries no path", ErrEffortModelMalformed)
	}
	if runtime.GOOS == "windows" {
		// Windows: `file:///C:/path` yields `Path=/C:/path`;
		// strip the leading slash so `C:/path` lands.
		path = strings.TrimPrefix(path, "/")
		// Also tolerate `file://C:/path` (no triple-slash),
		// which yields `Host=C:` and `Path=/path` -- glue
		// them together. The .NET-style operator habit of
		// omitting the triple slash is widespread.
		if u.Host != "" && strings.HasSuffix(u.Host, ":") {
			path = u.Host + "/" + strings.TrimPrefix(u.Path, "/")
		}
	}
	return path, nil
}

// looksLikeBarePath returns true when `raw` is unambiguously
// a local filesystem path (not a URI). The check is
// conservative -- only the two clearly-bare cases:
//
//   - POSIX absolute path: starts with `/` and does NOT have
//     a `://` separator anywhere.
//   - Windows drive-letter path: matches `[A-Za-z]:[/\\]...`
//     (`C:\foo`, `C:/foo`, `d:\bar`).
//
// Relative paths and UNC paths (`\\server\share`) also
// short-circuit to "bare" because they cannot be URIs.
// Any input that is not unambiguously bare falls through to
// [url.Parse] below.
func looksLikeBarePath(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	// UNC paths and Windows backslash paths.
	if strings.HasPrefix(raw, `\\`) || strings.Contains(raw, `\`) {
		return true
	}
	// Windows drive-letter path: `C:/...` or `C:\...`.
	if len(raw) >= 3 && raw[1] == ':' &&
		((raw[0] >= 'A' && raw[0] <= 'Z') || (raw[0] >= 'a' && raw[0] <= 'z')) &&
		(raw[2] == '/' || raw[2] == '\\') {
		return true
	}
	// POSIX absolute path.
	if strings.HasPrefix(raw, "/") {
		return true
	}
	// Relative paths (`./model.json`, `model.json`,
	// `models/v1.json`) -- treat as bare. A URL with no
	// scheme and no leading slash is indistinguishable from
	// a relative path; we prefer the "operator typed a
	// path" interpretation.
	return true
}

// -----------------------------------------------------------------------------
// Static estimator -- the "no model loaded" placeholder
// -----------------------------------------------------------------------------

// ZeroEffortEstimator is the trivial [EffortEstimator] that
// always returns `(0.0, nil)`. The composition root SHOULD
// NOT wire this in production (it is equivalent to NOT
// wiring an estimator at all); it exists so tests can pin
// the Stage 8.2 placeholder behaviour explicitly when
// asserting that [WithEffortEstimator] respects an injected
// estimator. The receiver is a value type so callers can
// pass `ZeroEffortEstimator{}` without heap allocation.
type ZeroEffortEstimator struct{}

// Estimate implements [EffortEstimator] -- always returns 0.
func (ZeroEffortEstimator) Estimate(_ RefactorTask, _ HotSpot, _ PolicySnapshot) (float64, error) {
	return 0, nil
}
