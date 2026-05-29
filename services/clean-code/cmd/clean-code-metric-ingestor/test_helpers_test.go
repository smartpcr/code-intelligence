package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mustGET constructs an HTTP GET request and fails the test if the
// stdlib NewRequest constructor errors.
func mustGET(t *testing.T, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, path, nil)
}

// newRecorder is a tiny alias so the test files don't repeat the
// `httptest.NewRecorder()` boilerplate.
func newRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

// testIsolationBundle constructs an [isolationBundle] for a test
// using `mgmtDB` as the management-role handle. The PG-backed
// repo store + appender constructors only validate non-nil at
// build time (no queries fire), so a `sqlmock.New()` handle is
// sufficient -- the bundle's coordinator + pool are entirely
// in-process; the bundle does NOT exercise mgmtDB at construction.
//
// PANICS via t.Fatalf on any error so call-sites read as
// fixture wiring, not error-handling boilerplate. The
// returned bundle is cleaned up via t.Cleanup so callers do
// NOT need to remember pool.Close().
func testIsolationBundle(t *testing.T, mgmtDB *sql.DB) *isolationBundle {
	t.Helper()
	iso, err := buildIsolation(mgmtDB)
	if err != nil {
		t.Fatalf("testIsolationBundle: buildIsolation: %v", err)
	}
	t.Cleanup(func() {
		_ = iso.Close()
	})
	return iso
}
