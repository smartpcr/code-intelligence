package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseVerbPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path    string
		ns      string
		verb    string
		ok      bool
		comment string
	}{
		{path: "/v1/mgmt/register_repo", ns: "mgmt", verb: "register_repo", ok: true},
		{path: "/v1/eval/gate", ns: "eval", verb: "gate", ok: true},
		{path: "/v1/policy/publish_rulepack", ns: "policy", verb: "publish_rulepack", ok: true},
		{path: "/v1/ingest/coverage", ns: "ingest", verb: "coverage", ok: true},
		{path: "/v1/mgmt/read.repo", ns: "mgmt", verb: "read.repo", ok: true, comment: "dotted verb allowed"},

		{path: "", ok: false, comment: "empty path"},
		{path: "/", ok: false, comment: "root path"},
		{path: "/v1/", ok: false, comment: "prefix only"},
		{path: "/v1/mgmt", ok: false, comment: "no verb segment"},
		{path: "/v1/mgmt/", ok: false, comment: "trailing slash on namespace"},
		{path: "/v1/mgmt/register_repo/", ok: false, comment: "trailing slash on verb"},
		{path: "/v1/mgmt/register_repo/extra", ok: false, comment: "extra segments"},
		{path: "/v2/mgmt/register_repo", ok: false, comment: "wrong version prefix"},
		{path: "/v1/MGMT/register_repo", ok: false, comment: "uppercase namespace"},
		{path: "/v1/mgmt/REGISTER_REPO", ok: false, comment: "uppercase verb"},
		{path: "/v1/mgmt/../etc", ok: false, comment: "path traversal"},
		{path: "/v1//register_repo", ok: false, comment: "empty namespace"},
		{path: "/v1/mgmt//", ok: false, comment: "empty verb segment"},
		{path: "v1/mgmt/x", ok: false, comment: "no leading slash"},
	}
	for _, tc := range cases {
		t.Run(tc.path+"::"+tc.comment, func(t *testing.T) {
			t.Parallel()
			ns, v, ok := ParseVerbPath(tc.path)
			if ok != tc.ok {
				t.Fatalf("ok=%v, want %v (%s)", ok, tc.ok, tc.comment)
			}
			if !tc.ok {
				return
			}
			if ns != tc.ns || v != tc.verb {
				t.Errorf("(ns,verb)=(%q,%q), want (%q,%q)", ns, v, tc.ns, tc.verb)
			}
		})
	}
}

func TestVerbRegistry_RegisterAndLookup(t *testing.T) {
	t.Parallel()
	reg := NewVerbRegistry()
	dummy := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	reg.Register(Verb{Namespace: "mgmt", Name: "register_repo", Handler: dummy})
	reg.Register(Verb{Namespace: "eval", Name: "gate", Handler: dummy})

	v, ok := reg.Lookup("mgmt", "register_repo")
	if !ok {
		t.Fatalf("Lookup(mgmt, register_repo) = ok=false; want true")
	}
	if v.DottedName() != "mgmt.register_repo" {
		t.Errorf("DottedName=%q, want mgmt.register_repo", v.DottedName())
	}
	if v.Path() != "/v1/mgmt/register_repo" {
		t.Errorf("Path=%q, want /v1/mgmt/register_repo", v.Path())
	}
	if _, ok := reg.Lookup("mgmt", "unknown"); ok {
		t.Errorf("Lookup(mgmt, unknown) = ok=true; want false")
	}
	if _, ok := reg.Lookup("unknown", "register_repo"); ok {
		t.Errorf("Lookup(unknown, register_repo) = ok=true; want false")
	}
}

func TestVerbRegistry_Verbs_DeterministicOrder(t *testing.T) {
	t.Parallel()
	reg := NewVerbRegistry()
	dummy := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	// Register out of order so the sort must reverse them.
	reg.Register(Verb{Namespace: "policy", Name: "publish", Handler: dummy})
	reg.Register(Verb{Namespace: "mgmt", Name: "register_repo", Handler: dummy})
	reg.Register(Verb{Namespace: "eval", Name: "gate", Handler: dummy})
	reg.Register(Verb{Namespace: "mgmt", Name: "set_mode", Handler: dummy})

	vs := reg.Verbs()
	want := []string{"eval.gate", "mgmt.register_repo", "mgmt.set_mode", "policy.publish"}
	if len(vs) != len(want) {
		t.Fatalf("Verbs len=%d, want %d", len(vs), len(want))
	}
	for i, v := range vs {
		if v.DottedName() != want[i] {
			t.Errorf("Verbs[%d]=%q, want %q", i, v.DottedName(), want[i])
		}
	}
}

func TestVerbRegistry_RegisterPanics(t *testing.T) {
	t.Parallel()
	dummy := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	cases := []struct {
		name   string
		v      Verb
		wantIn string
	}{
		{name: "empty-ns", v: Verb{Name: "x", Handler: dummy}, wantIn: "namespace token is empty"},
		{name: "empty-name", v: Verb{Namespace: "mgmt", Handler: dummy}, wantIn: "verb token is empty"},
		{name: "uppercase-ns", v: Verb{Namespace: "MGMT", Name: "x", Handler: dummy}, wantIn: "outside [a-z0-9_.-]"},
		{name: "slash-in-name", v: Verb{Namespace: "mgmt", Name: "a/b", Handler: dummy}, wantIn: "outside [a-z0-9_.-]"},
		{name: "dot-name", v: Verb{Namespace: "mgmt", Name: ".", Handler: dummy}, wantIn: "reserved path segment"},
		{name: "dotdot-ns", v: Verb{Namespace: "..", Name: "x", Handler: dummy}, wantIn: "reserved path segment"},
		{name: "nil-handler", v: Verb{Namespace: "mgmt", Name: "x"}, wantIn: "nil Handler"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := NewVerbRegistry()
			defer func() {
				rec := recover()
				if rec == nil {
					t.Fatalf("Register did not panic for %s", tc.name)
				}
				if !strings.Contains(stringify(rec), tc.wantIn) {
					t.Errorf("panic=%v, want substring %q", rec, tc.wantIn)
				}
			}()
			reg.Register(tc.v)
		})
	}
}

func TestVerbRegistry_DuplicateRegistrationPanics(t *testing.T) {
	t.Parallel()
	reg := NewVerbRegistry()
	dummy := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	reg.Register(Verb{Namespace: "mgmt", Name: "register_repo", Handler: dummy})
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatalf("duplicate Register did not panic")
		}
		if !strings.Contains(stringify(rec), "duplicate") {
			t.Errorf("panic=%v, want `duplicate` substring", rec)
		}
	}()
	reg.Register(Verb{Namespace: "mgmt", Name: "register_repo", Handler: dummy})
}

func TestVerbRegistry_RegisterDoesNotMountHandler(t *testing.T) {
	t.Parallel()
	// Registering a verb MUST NOT cause its handler to be
	// invoked. The handler is only invoked when an
	// authenticated request hits the gateway. This guards
	// the `unknown-verb-404` invariant -- if Register
	// itself ever triggered the handler we'd risk
	// accidental verb-side-effects at boot.
	called := false
	dummy := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true })
	reg := NewVerbRegistry()
	reg.Register(Verb{Namespace: "mgmt", Name: "register_repo", Handler: dummy})
	if called {
		t.Fatalf("Register invoked the handler")
	}
}

func TestVerbRegistry_Lookup_RepoIDExtractorThreadedThrough(t *testing.T) {
	t.Parallel()
	reg := NewVerbRegistry()
	dummy := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	reg.Register(Verb{
		Namespace: "mgmt",
		Name:      "register_repo",
		Handler:   dummy,
		RepoIDExtractor: func(r *http.Request) (string, *http.Request, error) {
			return r.Header.Get("X-Repo-ID"), r, nil
		},
	})
	v, ok := reg.Lookup("mgmt", "register_repo")
	if !ok {
		t.Fatalf("Lookup failed")
	}
	if v.RepoIDExtractor == nil {
		t.Fatalf("RepoIDExtractor lost on Register/Lookup roundtrip")
	}
	r := httptest.NewRequest("POST", "/v1/mgmt/register_repo", nil)
	r.Header.Set("X-Repo-ID", "repo-42")
	got, _, err := v.RepoIDExtractor(r)
	if err != nil {
		t.Fatalf("extractor returned err=%v", err)
	}
	if got != "repo-42" {
		t.Errorf("extracted repo_id=%q, want repo-42", got)
	}
}

func TestValidateNamespaceAndVerbName(t *testing.T) {
	t.Parallel()
	good := []string{"mgmt", "policy", "ingest", "eval", "mgmt.read", "a_b", "x-y", "lower9"}
	for _, s := range good {
		if err := ValidateNamespace(s); err != nil {
			t.Errorf("ValidateNamespace(%q) err=%v", s, err)
		}
		if err := ValidateVerbName(s); err != nil {
			t.Errorf("ValidateVerbName(%q) err=%v", s, err)
		}
	}
	bad := []string{"", "A", "Mgmt", "with space", "with/slash", ".", "..", "x?y", "tré"}
	for _, s := range bad {
		if err := ValidateNamespace(s); err == nil {
			t.Errorf("ValidateNamespace(%q) accepted", s)
		}
		if err := ValidateVerbName(s); err == nil {
			t.Errorf("ValidateVerbName(%q) accepted", s)
		}
	}
}

// stringify converts a recovered panic value to its string
// form for substring assertions. Most panics here are
// `string` (`panic(fmt.Sprintf(...))`); fall through to
// fmt-formatting for non-string panics.
func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return strings.TrimSpace(formatPanic(v))
}

func formatPanic(v any) string {
	type stringer interface{ Error() string }
	if e, ok := v.(stringer); ok {
		return e.Error()
	}
	return "<non-string panic>"
}
