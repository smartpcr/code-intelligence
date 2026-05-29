package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// PathPrefix is the canonical first segment every gateway-
// mounted verb lives under. Pinned at `/v1/` so a future
// breaking shape change ships under `/v2/` (per the
// architecture's API-versioning convention).
const PathPrefix = "/v1/"

// PathSeparator is the literal between `{namespace}` and
// `{verb}`. The gateway accepts ONLY `/` as the separator;
// the verb name MUST NOT contain a slash (enforced at
// registration time -- see [validateToken]).
const PathSeparator = "/"

// tokenRegexp is the strict allow-list for a `{namespace}` or
// `{verb}` path component. Lowercase alphanumerics, dot,
// underscore, hyphen -- matching the existing verb naming
// conventions (`mgmt.register_repo`, `policy.publish`,
// `eval.gate`). Uppercase / unicode / slash characters are
// rejected so registry tokens cannot collide with path-
// traversal segments (`.`, `..`) or with case-insensitive
// HTTP routing on case-sensitive backends.
var tokenRegexp = regexp.MustCompile(`^[a-z0-9_.\-]+$`)

// validateToken returns nil iff `token` matches [tokenRegexp]
// and is not one of the reserved path tokens (`.`, `..`). Any
// other shape is rejected with a descriptive error so the
// composition root's `Register` call panics loudly at
// startup.
func validateToken(role, token string) error {
	if token == "" {
		return fmt.Errorf("api: %s token is empty", role)
	}
	if token == "." || token == ".." {
		return fmt.Errorf("api: %s token %q is a reserved path segment", role, token)
	}
	if !tokenRegexp.MatchString(token) {
		return fmt.Errorf("api: %s token %q contains characters outside [a-z0-9_.-]", role, token)
	}
	return nil
}

// ValidateNamespace validates a `{namespace}` token. Exported
// so the composition root (and conformance tests) can run the
// same check against a static verb manifest before wiring.
func ValidateNamespace(ns string) error { return validateToken("namespace", ns) }

// ValidateVerbName validates a `{verb}` token (the last path
// component, not the dotted `<ns>.<verb>` form). Exported for
// the same reasons as [ValidateNamespace].
func ValidateVerbName(v string) error { return validateToken("verb", v) }

// Verb is one entry in the gateway's verb registry. Each Verb
// pins a `{namespace, name}` pair to a backing
// [http.Handler]. The gateway mounts the handler at
// `/v1/{namespace}/{name}` and forwards every authenticated
// request to it after stamping the `X-OIDC-Subject` header.
//
// # Why not import the handler signatures from the verb packages
//
// The gateway is a generic forwarder; it MUST NOT depend on
// the per-namespace types (`management.MgmtWriter`,
// `evaluator.GateHandler`, etc.) -- that would couple
// `internal/api` to every downstream verb implementation
// and re-create the surface bifurcation that
// `MgmtSurfaceRoutes` already cleaned up. Composition roots
// adapt each verb's HTTP handler into a [Verb] entry at
// boot.
type Verb struct {
	// Namespace is the first path segment after `/v1/`
	// (`mgmt`, `eval`, `policy`, `ingest`, ...). MUST
	// match [tokenRegexp].
	Namespace string
	// Name is the second path segment. MUST match
	// [tokenRegexp]. The on-wire name is the literal --
	// "verb names map 1:1 to HTTP path components" per
	// implementation-plan Stage 6.4 / architecture Sec
	// 6 line 1.
	Name string
	// Handler is the backing HTTP handler. The gateway
	// invokes it with a request whose `X-OIDC-Subject`
	// header has been authoritatively set from the
	// verified bearer token.
	Handler http.Handler
	// RepoIDExtractor is the optional hook the gateway
	// calls to populate the span's `repo_id` attribute.
	// MAY be nil (the span attribute defaults to ""). The
	// extractor is called AFTER auth + verb lookup
	// succeed, so the request it sees is always
	// authenticated.
	//
	// # Contract
	//
	//   - The first return value is the extracted repo_id
	//     (empty string when none is present and the
	//     extractor wants to surface "no repo_id" rather
	//     than failing).
	//   - The second return value is the request the
	//     gateway should forward. Extractors that peek
	//     the body MUST return a request with the body
	//     restored (e.g. via [io.NopCloser]); extractors
	//     that do NOT need to rewind MAY return the
	//     original `r` (or nil, treated as "use
	//     original").
	//   - The third return value is an extraction error.
	//     A non-nil error does NOT fail the request --
	//     the gateway logs the error and proceeds with
	//     repo_id="". The downstream verb handler is the
	//     authoritative validator of the body shape.
	//
	// A panic inside the extractor is captured by the
	// gateway's panic-recover defer and surfaced as a 500
	// with the panic recorded on the span (item #6 from
	// iter-1 evaluator feedback).
	RepoIDExtractor RepoIDExtractor
}

// DottedName returns the canonical `{namespace}.{name}` form
// of the verb -- the literal that lands on the span's
// `verb` attribute. Mirrors the existing verb-name
// convention (`mgmt.register_repo`, `policy.publish_rulepack`,
// `eval.gate`).
func (v Verb) DottedName() string {
	return v.Namespace + "." + v.Name
}

// Path returns the canonical mount path the gateway uses for
// the verb. Mirrors `/v1/{namespace}/{verb}` verbatim.
func (v Verb) Path() string {
	return PathPrefix + v.Namespace + PathSeparator + v.Name
}

// VerbRegistry is the composition-root-owned table mapping
// `(namespace, name)` pairs to [Verb] entries. The gateway
// looks up the verb on each request; an entry not in the
// registry returns 404.
//
// The registry is safe for concurrent reads after
// composition (a single writer thread populates it at
// startup); concurrent Register calls during request serving
// are not supported -- the composition root MUST register
// all verbs before [Server.ListenAndServe] returns.
type VerbRegistry struct {
	mu    sync.RWMutex
	verbs map[string]Verb // key = `<namespace>/<name>`
}

// NewVerbRegistry constructs an empty registry. Composition
// roots call [VerbRegistry.Register] for each verb they
// want exposed.
func NewVerbRegistry() *VerbRegistry {
	return &VerbRegistry{verbs: map[string]Verb{}}
}

// Register adds `v` to the registry. PANICS on:
//
//   - empty / malformed namespace or name (see [tokenRegexp]);
//   - duplicate `(namespace, name)` pair (re-registration is
//     almost always a wiring bug);
//   - nil Handler.
//
// Panicking at startup beats silently shadowing a previously
// registered verb (which would route requests to the wrong
// backend). The composition root catches the panic in its
// boot sequence and logs the wiring error.
func (r *VerbRegistry) Register(v Verb) {
	if err := ValidateNamespace(v.Namespace); err != nil {
		panic(fmt.Sprintf("api.VerbRegistry.Register: %v", err))
	}
	if err := ValidateVerbName(v.Name); err != nil {
		panic(fmt.Sprintf("api.VerbRegistry.Register: %v", err))
	}
	if v.Handler == nil {
		panic(fmt.Sprintf("api.VerbRegistry.Register: nil Handler for %s.%s", v.Namespace, v.Name))
	}
	key := v.Namespace + PathSeparator + v.Name
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.verbs[key]; dup {
		panic(fmt.Sprintf("api.VerbRegistry.Register: duplicate verb registration for %s.%s", v.Namespace, v.Name))
	}
	r.verbs[key] = v
}

// Lookup returns the verb registered for the
// `(namespace, name)` pair, or `ok=false` when none is
// registered. The gateway calls Lookup AFTER verb-shape
// validation; the workstream brief pins unknown verbs to
// return 404 regardless of whether the caller is
// authenticated.
func (r *VerbRegistry) Lookup(namespace, name string) (Verb, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.verbs[namespace+PathSeparator+name]
	return v, ok
}

// Replace updates an already-registered verb's [Handler] (and
// optionally its [RepoIDExtractor]) in place. PANICS when
// the dotted name `{namespace}.{name}` is not yet
// registered, when the new handler is nil, or when the
// dotted name does not split on a single dot. Composition
// roots use Replace to swap the placeholder
// `notWiredHandler` mounted by [NewDefaultRegistry] for the
// real verb handler.
//
// Replace ALWAYS replaces the handler. The RepoIDExtractor
// is replaced only when `extractor != nil`; pass nil to
// preserve the existing extractor (typical when the
// composition root accepts the architecture's default
// source).
func (r *VerbRegistry) Replace(dottedName string, handler http.Handler, extractor RepoIDExtractor) {
	if handler == nil {
		panic(fmt.Sprintf("api.VerbRegistry.Replace: nil Handler for %s", dottedName))
	}
	dot := -1
	for i := 0; i < len(dottedName); i++ {
		if dottedName[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot >= len(dottedName)-1 {
		panic(fmt.Sprintf("api.VerbRegistry.Replace: %q is not a {namespace}.{name} dotted verb", dottedName))
	}
	ns := dottedName[:dot]
	// For dotted verbs like `mgmt.read.repo` the verb
	// token IS `read.repo` (everything after the FIRST
	// dot). Match the registry's key shape.
	name := dottedName[dot+1:]
	key := ns + PathSeparator + name
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.verbs[key]
	if !ok {
		panic(fmt.Sprintf("api.VerbRegistry.Replace: %q is not registered (call Register first)", dottedName))
	}
	existing.Handler = handler
	if extractor != nil {
		existing.RepoIDExtractor = extractor
	}
	r.verbs[key] = existing
}

// Verbs returns a snapshot of every registered verb in
// deterministic order (sorted by dotted name). Used by the
// composition-root smoke test and by `/v1/_meta/verbs` if
// the operator wires that discovery surface.
func (r *VerbRegistry) Verbs() []Verb {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Verb, 0, len(r.verbs))
	for _, v := range r.verbs {
		out = append(out, v)
	}
	// Sort by dotted name so the slice is deterministic.
	// Insertion-sort because the verb count is bounded
	// (~20 verbs) and we avoid importing `sort` for one
	// call.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].DottedName() > out[j].DottedName(); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ParseVerbPath splits a request URL path into its
// `(namespace, verb)` components. Returns `ok=false` when
// the path is not exactly `/v1/{namespace}/{verb}` -- the
// gateway maps that to 404.
//
// The path MUST be exactly three segments after the leading
// slash (`v1`, `<namespace>`, `<verb>`); a trailing slash, a
// missing segment, an empty segment, or extra segments all
// return ok=false. Both tokens MUST satisfy [tokenRegexp];
// uppercase characters or `.` / `..` traversal tokens
// return ok=false -- the registry can never accept those
// per [validateToken].
func ParseVerbPath(urlPath string) (namespace, verb string, ok bool) {
	if !strings.HasPrefix(urlPath, PathPrefix) {
		return "", "", false
	}
	rest := urlPath[len(PathPrefix):]
	// Reject trailing slash explicitly -- `/v1/mgmt/foo/`
	// SHOULD not match `/v1/mgmt/foo`; the gateway pins
	// the canonical form and rejects equivalent-looking
	// trailing-slash forms so a future routing change
	// does not silently match both.
	if strings.HasSuffix(rest, PathSeparator) {
		return "", "", false
	}
	parts := strings.Split(rest, PathSeparator)
	if len(parts) != 2 {
		return "", "", false
	}
	ns, vb := parts[0], parts[1]
	if ValidateNamespace(ns) != nil {
		return "", "", false
	}
	if ValidateVerbName(vb) != nil {
		return "", "", false
	}
	return ns, vb, true
}
