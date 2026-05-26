package main

import (
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
