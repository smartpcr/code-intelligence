package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// CanonicalVerb is the architecture-pinned definition of a
// single verb the gateway routes. Each canonical verb names
// the namespace + verb token, the canonical repo_id source
// (so a built-in extractor can be wired without per-verb
// custom code), and a short description that lands in the
// gateway's `/v1/_meta/verbs` discovery surface.
//
// The table below is the SOURCE OF TRUTH the gateway uses to
// satisfy the workstream brief verbatim: "expose every verb
// at /v1/{namespace}/{verb}". The composition root replaces
// each canonical entry's handler with the production wiring
// via [VerbRegistry.Replace]; until replaced, the gateway
// returns 503 from a stub handler so the route exists but
// signals "subsystem not wired".
//
// # Why a centralised table
//
// Without a single source of truth, "every verb is mounted"
// degenerates into a brittle ad-hoc list across compositions
// roots. Centralising the table here:
//
//   - Lets `TestDefaultRegistry_MountsEveryCanonicalVerb`
//     assert exhaustively against architecture Sec 6.2-6.5
//     without re-typing the verb names.
//   - Lets a future stage drift-check the architecture
//     document against the code (a verb appearing in
//     architecture.md must appear here, and vice versa).
//   - Lets the composition root iterate the canonical
//     verbs and wire a real handler per dotted name --
//     forgetting one surfaces as a fast 503, not as a
//     silent 404.
type CanonicalVerb struct {
	// Namespace is the first path segment ("mgmt",
	// "policy", "eval", "ingest").
	Namespace string
	// Name is the second path segment (e.g.
	// "register_repo", "publish_rulepack").
	Name string
	// RepoIDSource describes where the canonical
	// `repo_id` value lives for this verb. The default
	// registry picks an extractor based on this enum
	// (JSON body, query parameter, header, none).
	RepoIDSource RepoIDSource
	// JSONBodyPath is the nested JSON object path the
	// extractor walks before reading the leaf `repo_id`.
	// Only consulted when RepoIDSource == RepoIDFromJSONBody.
	// Empty path (default) uses the top-level field
	// [DefaultRepoIDJSONField]; non-empty path uses
	// [NestedJSONBodyRepoIDExtractor] with the supplied
	// keys (e.g. `mgmt.override` uses
	// `["scope_filter","repo_id"]` per
	// `internal/policy/steward/types.go`).
	JSONBodyPath []string
	// Description is a one-line human summary used by
	// the `/v1/_meta/verbs` discovery surface (and the
	// composition root's startup log).
	Description string
}

// DottedName returns `{namespace}.{name}`.
func (c CanonicalVerb) DottedName() string {
	return c.Namespace + "." + c.Name
}

// Path returns the canonical `/v1/{namespace}/{name}` URL.
func (c CanonicalVerb) Path() string {
	return PathPrefix + c.Namespace + PathSeparator + c.Name
}

// RepoIDSource enumerates where in a request the canonical
// `repo_id` lives. Pinned as a typed enum so a verb
// definition explicitly states its repo_id source rather
// than carrying a free-form extractor function (which would
// drift across versions).
type RepoIDSource int

const (
	// RepoIDFromJSONBody pulls `repo_id` from the
	// request's JSON body using
	// [JSONBodyRepoIDExtractor].
	RepoIDFromJSONBody RepoIDSource = iota
	// RepoIDFromQuery pulls `repo_id` from the URL
	// query parameter using [QueryRepoIDExtractor].
	RepoIDFromQuery
	// RepoIDFromHeader pulls `repo_id` from the
	// request's `X-Forge-Repo-ID` header. Used by the
	// header-borne ingest wire shapes.
	RepoIDFromHeader
	// RepoIDNone indicates this verb does not carry a
	// repo_id (e.g. mgmt.retract_sample uses
	// sample_id; policy.activate uses
	// policy_version_id). The span attribute is "".
	RepoIDNone
)

// repoIDForwardHeader is the header name canonical
// ingest-shape verbs use to carry the repo identifier (per
// internal/ingest/webhook/verb_handler.go's RepoIDHeader).
const repoIDForwardHeader = "X-Forge-Repo-ID"

// ExtractorFor returns the built-in [RepoIDExtractor] that
// implements the verb's declared [RepoIDSource]. For
// `RepoIDFromJSONBody` verbs, a non-empty `JSONBodyPath`
// selects [NestedJSONBodyRepoIDExtractor]; an empty path
// uses [JSONBodyRepoIDExtractor] on the top-level repo_id
// field.
func (c CanonicalVerb) ExtractorFor() RepoIDExtractor {
	switch c.RepoIDSource {
	case RepoIDFromJSONBody:
		if len(c.JSONBodyPath) > 0 {
			return NestedJSONBodyRepoIDExtractor(DefaultRepoIDPeekBytes, c.JSONBodyPath...)
		}
		return JSONBodyRepoIDExtractor(DefaultRepoIDJSONField, DefaultRepoIDPeekBytes)
	case RepoIDFromQuery:
		return QueryRepoIDExtractor(DefaultRepoIDQueryParam)
	case RepoIDFromHeader:
		return HeaderRepoIDExtractor(repoIDForwardHeader)
	case RepoIDNone:
		return NoRepoIDExtractor
	default:
		return NoRepoIDExtractor
	}
}

// CanonicalVerbs is the architecture-pinned list of every
// verb the gateway exposes. The list mirrors architecture
// Sec 6.2-6.5 (`eval.*`, `mgmt.*`, `ingest.*`, `policy.*`)
// verbatim. Each entry's RepoIDSource is taken from the
// wire-shape definitions in `internal/management` and
// `internal/ingest/webhook` (see the doc-comment on the
// matching verb implementation for the canonical request
// JSON shape).
//
// Adding a verb here is a deliberate act -- the
// drift-check test in defaults_test.go fails if a verb
// appears in architecture.md but not here.
var CanonicalVerbs = []CanonicalVerb{
	// 6.2 eval.*
	{
		Namespace: "eval", Name: "gate", RepoIDSource: RepoIDFromJSONBody,
		Description: "Evaluate a SHA against the active policy; returns verdict + findings.",
	},

	// 6.3 mgmt.* read verbs (single repo_id query parameter).
	{Namespace: "mgmt", Name: "read.repo", RepoIDSource: RepoIDFromQuery, Description: "Read a repo by id."},
	{Namespace: "mgmt", Name: "read.metric_sample", RepoIDSource: RepoIDNone, Description: "Read a metric sample by sample_id."},
	{Namespace: "mgmt", Name: "read.metric_samples", RepoIDSource: RepoIDFromQuery, Description: "List metric samples for repo+sha."},
	{Namespace: "mgmt", Name: "read.findings", RepoIDSource: RepoIDFromQuery, Description: "List findings for repo (optional sha, severity)."},
	{Namespace: "mgmt", Name: "read.regressions", RepoIDSource: RepoIDFromQuery, Description: "List newly-failing rules for repo at sha vs prev."},
	{Namespace: "mgmt", Name: "read.cross_repo", RepoIDSource: RepoIDFromQuery, Description: "Cross-repo percentile + histogram for metric/scope."},
	{Namespace: "mgmt", Name: "read.portfolio", RepoIDSource: RepoIDNone, Description: "Portfolio snapshot for metric_kind+scope_kind."},
	{Namespace: "mgmt", Name: "read.refactor_plan", RepoIDSource: RepoIDFromQuery, Description: "Read refactor plan for repo+sha."},

	// 6.3 mgmt.* write verbs (JSON body carries repo_id).
	{Namespace: "mgmt", Name: "register_repo", RepoIDSource: RepoIDFromJSONBody, Description: "Onboard a new repo; writes repo_event(registered)."},
	{Namespace: "mgmt", Name: "set_mode", RepoIDSource: RepoIDFromJSONBody, Description: "Toggle repo mode; writes repo_event(mode_changed)."},
	{Namespace: "mgmt", Name: "retract_sample", RepoIDSource: RepoIDNone, Description: "Retract a metric sample by sample_id."},
	{Namespace: "mgmt", Name: "rescan", RepoIDSource: RepoIDFromJSONBody, Description: "Enqueue a fresh full scan for repo+sha."},
	// mgmt.override: `repo_id` lives at body.scope_filter.repo_id
	// per internal/policy/steward/types.go OverrideRequest /
	// ScopeFilter. Item #3 from iter-2 evaluator feedback.
	{
		Namespace: "mgmt", Name: "override", RepoIDSource: RepoIDFromJSONBody,
		JSONBodyPath: []string{"scope_filter", "repo_id"},
		Description:  "Operator mute/unmute of a rule (rule_id); scope_filter.repo_id binds attribution.",
	},

	// 6.4 ingest.* webhooks (CI publishers). NOTE: these
	// verbs run their own HMAC auth in production via
	// `internal/ingest/webhook/router.go`; mounting them
	// here on the OIDC gateway gives the OPERATOR a
	// REST/JSON path for the same shapes (e.g. replaying
	// a stuck CI payload by hand). Per-verb auth
	// override is a Stage 6.5 follow-up; for now the
	// gateway requires OIDC for ALL verbs (CI bots use
	// the dedicated /v1/ingest/ HMAC router).
	//
	// The repo_id source mirrors each verb's
	// ExtractMetadata implementation:
	//   - coverage / test_balance: header `X-Forge-Repo-ID`
	//     (per webhook router metadata-from-header binding).
	//   - churn / defects: JSON body field `repo_id` (per
	//     `internal/ingest/webhook/{churn,defects}_verb.go`
	//     ExtractMetadata, which decodes the body and reads
	//     `payload.RepoID` -- the header is explicitly
	//     ignored for these verbs).
	// Item #2 from iter-2 evaluator feedback.
	{Namespace: "ingest", Name: "coverage", RepoIDSource: RepoIDFromHeader, Description: "Ingest Cobertura coverage payload (sha-bound; X-Forge-Repo-ID header)."},
	{Namespace: "ingest", Name: "test_balance", RepoIDSource: RepoIDFromHeader, Description: "Ingest test-balance payload (sha-bound; X-Forge-Repo-ID header)."},
	{Namespace: "ingest", Name: "churn", RepoIDSource: RepoIDFromJSONBody, Description: "Ingest churn payload (per-row sha; repo_id in JSON body)."},
	{Namespace: "ingest", Name: "defects", RepoIDSource: RepoIDFromJSONBody, Description: "Ingest defect payload (per-row sha; repo_id in JSON body)."},

	// 6.5 policy.*
	{Namespace: "policy", Name: "publish", RepoIDSource: RepoIDNone, Description: "Publish a new PolicyVersion (signed)."},
	{Namespace: "policy", Name: "activate", RepoIDSource: RepoIDNone, Description: "Activate a published PolicyVersion."},
	{Namespace: "policy", Name: "publish_rulepack", RepoIDSource: RepoIDNone, Description: "Publish a new RulePack."},
	{Namespace: "policy", Name: "keys.list_active", RepoIDSource: RepoIDNone, Description: "List active signing keys (read-only)."},
}

// notWiredHandler is the placeholder [http.Handler] mounted
// for every canonical verb the composition root has not yet
// replaced with a real handler. Returns 503 with a JSON
// envelope carrying the dotted verb name so a probing
// operator sees "I know this verb exists, the subsystem is
// not wired in this deployment" rather than "404 typo".
type notWiredHandler struct {
	verb string
}

func (h notWiredHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Error: fmt.Sprintf("verb %q is registered but its backing subsystem is not wired in this deployment", h.verb),
		Code:  "VERB_NOT_WIRED",
	})
}

// NewDefaultRegistry constructs a [VerbRegistry] pre-populated
// with every canonical verb (architecture Sec 6.2-6.5). Each
// verb is mounted with a [notWiredHandler] stub the
// composition root replaces via [VerbRegistry.Replace] when
// it wires the production backing handler.
//
// The registry returned is safe to use immediately: probing
// any canonical verb path returns 503 (subsystem not
// wired), and probing an unknown verb returns 404 (verb does
// not exist in the architecture).
//
// Typical composition-root usage:
//
//	reg := api.NewDefaultRegistry()
//	reg.Replace("mgmt.register_repo", mgmtWriter.RegisterRepo)
//	reg.Replace("mgmt.set_mode", mgmtWriter.SetMode)
//	// ... wire every verb the deployment actually serves ...
//	srv := api.NewServer(api.ServerConfig{Authenticator: oidc, Registry: reg})
func NewDefaultRegistry() *VerbRegistry {
	reg := NewVerbRegistry()
	for _, cv := range CanonicalVerbs {
		reg.Register(Verb{
			Namespace:       cv.Namespace,
			Name:            cv.Name,
			Handler:         notWiredHandler{verb: cv.DottedName()},
			RepoIDExtractor: cv.ExtractorFor(),
		})
	}
	return reg
}

// CanonicalVerbNames returns the dotted names of every
// canonical verb in deterministic order. Used by the
// composition-root smoke test and by the
// `/v1/_meta/verbs` discovery surface.
func CanonicalVerbNames() []string {
	out := make([]string, 0, len(CanonicalVerbs))
	for _, cv := range CanonicalVerbs {
		out = append(out, cv.DottedName())
	}
	sort.Strings(out)
	return out
}

// Wiring carries one optional [http.Handler] per canonical
// verb the gateway exposes. A composition root populates
// Wiring with handlers ADAPTED FROM the existing
// per-namespace implementations (the typed handler structs
// living in `internal/management/`, `internal/evaluator/`,
// `internal/ingest/webhook/`, `internal/policy/steward/`,
// ...) and passes the Wiring to [NewWiredRegistry]; a
// non-nil slot REPLACES the [notWiredHandler] stub the
// architecture-default registry installed. nil slots stay
// 503 (`VERB_NOT_WIRED`) so an incomplete wiring surfaces
// as a fast diagnostic rather than a silent 404.
//
// # Why an explicit slot per verb (not a map)
//
// A map[string]http.Handler would be terser but loses two
// guarantees:
//
//  1. The Wiring struct's field names are CONSTANTS the
//     compiler validates -- a typo (`SetMmode` instead of
//     `SetMode`) is a compile error, not a runtime 404.
//  2. The Wiring struct IS the contract: a future composition
//     root that drops a verb sees the field stay in the
//     struct (as nil) rather than disappearing silently;
//     the architecture's "every verb exposed" invariant
//     stays grep-able from the wiring source code.
//
// # Why the api package does not import sibling packages
//
// The gateway is a generic forwarder. Importing the
// per-namespace handler packages (`internal/management`,
// `internal/evaluator`, ...) directly would recreate the
// circular-coupling surface bifurcation `MgmtSurfaceRoutes`
// already cleaned up. Wiring decouples the gateway from the
// concrete handler types -- the composition root is the only
// place that knows about both ends, and the adapter shim per
// verb is typically one line (e.g.
// `EvalGate: evaluator.NewGateHTTPHandler(deps)`).
//
// # Verbs covered
//
// One field per [CanonicalVerb], named in CamelCase from the
// dotted verb form (so `mgmt.read.repo` -> `MgmtReadRepo`).
// The exhaustive list mirrors architecture Sec 6.2-6.5.
type Wiring struct {
	// 6.2 eval.*
	EvalGate http.Handler

	// 6.3 mgmt.* reads
	MgmtReadRepo          http.Handler
	MgmtReadMetricSample  http.Handler
	MgmtReadMetricSamples http.Handler
	MgmtReadFindings      http.Handler
	MgmtReadRegressions   http.Handler
	MgmtReadCrossRepo     http.Handler
	MgmtReadPortfolio     http.Handler
	MgmtReadRefactorPlan  http.Handler

	// 6.3 mgmt.* writes
	MgmtRegisterRepo  http.Handler
	MgmtSetMode       http.Handler
	MgmtRetractSample http.Handler
	MgmtRescan        http.Handler
	MgmtOverride      http.Handler

	// 6.4 ingest.* webhooks
	IngestCoverage    http.Handler
	IngestTestBalance http.Handler
	IngestChurn       http.Handler
	IngestDefects     http.Handler

	// 6.5 policy.*
	PolicyPublish        http.Handler
	PolicyActivate       http.Handler
	PolicyPublishRulepack http.Handler
	PolicyKeysListActive  http.Handler
}

// wiringSlot maps a canonical dotted verb name to its
// Wiring field accessor. The slice is package-private and
// constructed once; [NewWiredRegistry] iterates it to find
// non-nil handlers for each canonical verb.
type wiringSlot struct {
	dottedName string
	pick       func(*Wiring) http.Handler
}

var wiringSlots = []wiringSlot{
	{"eval.gate", func(w *Wiring) http.Handler { return w.EvalGate }},

	{"mgmt.read.repo", func(w *Wiring) http.Handler { return w.MgmtReadRepo }},
	{"mgmt.read.metric_sample", func(w *Wiring) http.Handler { return w.MgmtReadMetricSample }},
	{"mgmt.read.metric_samples", func(w *Wiring) http.Handler { return w.MgmtReadMetricSamples }},
	{"mgmt.read.findings", func(w *Wiring) http.Handler { return w.MgmtReadFindings }},
	{"mgmt.read.regressions", func(w *Wiring) http.Handler { return w.MgmtReadRegressions }},
	{"mgmt.read.cross_repo", func(w *Wiring) http.Handler { return w.MgmtReadCrossRepo }},
	{"mgmt.read.portfolio", func(w *Wiring) http.Handler { return w.MgmtReadPortfolio }},
	{"mgmt.read.refactor_plan", func(w *Wiring) http.Handler { return w.MgmtReadRefactorPlan }},

	{"mgmt.register_repo", func(w *Wiring) http.Handler { return w.MgmtRegisterRepo }},
	{"mgmt.set_mode", func(w *Wiring) http.Handler { return w.MgmtSetMode }},
	{"mgmt.retract_sample", func(w *Wiring) http.Handler { return w.MgmtRetractSample }},
	{"mgmt.rescan", func(w *Wiring) http.Handler { return w.MgmtRescan }},
	{"mgmt.override", func(w *Wiring) http.Handler { return w.MgmtOverride }},

	{"ingest.coverage", func(w *Wiring) http.Handler { return w.IngestCoverage }},
	{"ingest.test_balance", func(w *Wiring) http.Handler { return w.IngestTestBalance }},
	{"ingest.churn", func(w *Wiring) http.Handler { return w.IngestChurn }},
	{"ingest.defects", func(w *Wiring) http.Handler { return w.IngestDefects }},

	{"policy.publish", func(w *Wiring) http.Handler { return w.PolicyPublish }},
	{"policy.activate", func(w *Wiring) http.Handler { return w.PolicyActivate }},
	{"policy.publish_rulepack", func(w *Wiring) http.Handler { return w.PolicyPublishRulepack }},
	{"policy.keys.list_active", func(w *Wiring) http.Handler { return w.PolicyKeysListActive }},
}

// NewWiredRegistry constructs a [VerbRegistry] pre-populated
// with every canonical verb (architecture Sec 6.2-6.5).
// Each non-nil [http.Handler] in `wiring` REPLACES the
// architecture-default [notWiredHandler] stub for the
// matching verb; nil slots remain mounted with the 503
// stub.
//
// Composition-root pattern (the api package does NOT import
// the per-namespace packages -- see [Wiring]):
//
//	mgmtRouter := management.NewMgmtSurfaceRoutes(deps)
//	reg := api.NewWiredRegistry(api.Wiring{
//	    MgmtRegisterRepo: mgmtRouter.HTTPHandler("register_repo"),
//	    MgmtSetMode:      mgmtRouter.HTTPHandler("set_mode"),
//	    // ...
//	    EvalGate:         evaluator.NewGateHTTPHandler(deps),
//	    IngestCoverage:   webhookRouter.HTTPHandler("coverage"),
//	    PolicyPublish:    policyHTTP.Publish,
//	})
//	srv := api.NewServer(api.ServerConfig{
//	    Authenticator: oidcAuth,
//	    Registry:      reg,
//	    Tracer:        api.NewOTelTracerFromGlobal(),
//	})
//
// Slots left nil are reported by [Wiring.MissingVerbs] so
// the composition root can log a startup warning.
//
// PANICS when a non-canonical verb name appears in
// `wiringSlots` (a build-time assertion against drift from
// architecture.md).
func NewWiredRegistry(wiring Wiring) *VerbRegistry {
	reg := NewDefaultRegistry()
	w := wiring
	for _, slot := range wiringSlots {
		h := slot.pick(&w)
		if h == nil {
			continue
		}
		reg.Replace(slot.dottedName, h, nil)
	}
	return reg
}

// MissingVerbs returns the dotted names of every canonical
// verb whose Wiring slot is nil. Useful for the composition
// root's boot-time validation (`if missing :=
// wiring.MissingVerbs(); len(missing) > 0 { logger.Warn(...) }`).
func (w Wiring) MissingVerbs() []string {
	out := make([]string, 0)
	for _, slot := range wiringSlots {
		if slot.pick(&w) == nil {
			out = append(out, slot.dottedName)
		}
	}
	sort.Strings(out)
	return out
}

// WiredVerbs returns the dotted names of every canonical
// verb whose Wiring slot is non-nil. Useful for boot-time
// observability ("served verbs: ...").
func (w Wiring) WiredVerbs() []string {
	out := make([]string, 0)
	for _, slot := range wiringSlots {
		if slot.pick(&w) != nil {
			out = append(out, slot.dottedName)
		}
	}
	sort.Strings(out)
	return out
}

// Validate asserts that every dotted name in [wiringSlots]
// is an architecture-pinned canonical verb. Used by the
// init-time assertion below to catch drift between
// [CanonicalVerbs] and [wiringSlots].
func validateWiringSlots() error {
	canonical := make(map[string]struct{}, len(CanonicalVerbs))
	for _, cv := range CanonicalVerbs {
		canonical[cv.DottedName()] = struct{}{}
	}
	for _, slot := range wiringSlots {
		if _, ok := canonical[slot.dottedName]; !ok {
			return fmt.Errorf("api: wiringSlots contains %q which is not in CanonicalVerbs", slot.dottedName)
		}
	}
	if len(wiringSlots) != len(CanonicalVerbs) {
		return fmt.Errorf("api: wiringSlots has %d entries, CanonicalVerbs has %d", len(wiringSlots), len(CanonicalVerbs))
	}
	return nil
}

func init() {
	if err := validateWiringSlots(); err != nil {
		panic(err.Error())
	}
}
