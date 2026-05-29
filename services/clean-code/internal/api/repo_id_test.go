package api

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNoRepoIDExtractor_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("POST", "/v1/mgmt/retract_sample", strings.NewReader(`{"sample_id":"abc"}`))
	repoID, out, err := NoRepoIDExtractor(req)
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if repoID != "" {
		t.Errorf("repoID=%q, want empty", repoID)
	}
	if out != req {
		t.Errorf("returned request differs from input")
	}
}

func TestHeaderRepoIDExtractor(t *testing.T) {
	t.Parallel()
	ext := HeaderRepoIDExtractor("X-Forge-Repo-ID")
	req := httptest.NewRequest("POST", "/v1/ingest/coverage", nil)
	req.Header.Set("X-Forge-Repo-ID", "repo-7")
	repoID, _, err := ext(req)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if repoID != "repo-7" {
		t.Errorf("repoID=%q, want repo-7", repoID)
	}
	// Missing header -> empty + nil error
	req2 := httptest.NewRequest("POST", "/v1/ingest/coverage", nil)
	repoID2, _, err := ext(req2)
	if err != nil || repoID2 != "" {
		t.Errorf("missing-header: got (%q, %v), want (\"\", nil)", repoID2, err)
	}
}

func TestQueryRepoIDExtractor(t *testing.T) {
	t.Parallel()
	ext := QueryRepoIDExtractor("repo_id")
	req := httptest.NewRequest("GET", "/v1/mgmt/read.repo?repo_id=repo-42", nil)
	repoID, _, err := ext(req)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if repoID != "repo-42" {
		t.Errorf("repoID=%q, want repo-42", repoID)
	}
}

func TestJSONBodyRepoIDExtractor_ExtractsAndRestoresBody(t *testing.T) {
	t.Parallel()
	body := `{"repo_id":"repo-7","sha":"abc"}`
	req := httptest.NewRequest("POST", "/v1/mgmt/rescan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := JSONBodyRepoIDExtractor("", 0)
	repoID, out, err := ext(req)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if repoID != "repo-7" {
		t.Errorf("repoID=%q, want repo-7", repoID)
	}
	got, _ := io.ReadAll(out.Body)
	if string(got) != body {
		t.Errorf("restored body=%q, want %q", got, body)
	}
}

func TestJSONBodyRepoIDExtractor_NonJSONContentTypeSkipped(t *testing.T) {
	t.Parallel()
	body := `repo_id=repo-7`
	req := httptest.NewRequest("POST", "/v1/mgmt/rescan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ext := JSONBodyRepoIDExtractor("repo_id", 1024)
	repoID, _, err := ext(req)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if repoID != "" {
		t.Errorf("non-JSON should return empty, got %q", repoID)
	}
}

func TestJSONBodyRepoIDExtractor_OversizedBody(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 200) // > maxBytes
	body := `{"repo_id":"repo-7","junk":"` + big + `"}`
	req := httptest.NewRequest("POST", "/v1/mgmt/rescan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := JSONBodyRepoIDExtractor("repo_id", 64)
	repoID, out, err := ext(req)
	if err == nil {
		t.Fatalf("oversized body should return extraction error")
	}
	if repoID != "" {
		t.Errorf("oversized body must not yield repo_id, got %q", repoID)
	}
	// Item #4 fix: the restored body must contain the COMPLETE
	// original payload, not just the truncated peek prefix.
	// Otherwise downstream handlers see a corrupted JSON
	// document (oversized requests would still be forwarded
	// but with the tail of the body chopped off).
	got, readErr := io.ReadAll(out.Body)
	if readErr != nil {
		t.Fatalf("read restored body: %v", readErr)
	}
	if string(got) != body {
		t.Errorf("restored body diverged from original\n  got:  %q\n  want: %q", got, body)
	}
}

func TestJSONBodyRepoIDExtractor_MalformedJSON(t *testing.T) {
	t.Parallel()
	body := `{"repo_id":`
	req := httptest.NewRequest("POST", "/v1/mgmt/rescan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := JSONBodyRepoIDExtractor("repo_id", 1024)
	_, out, err := ext(req)
	if err == nil {
		t.Fatalf("malformed JSON should return extraction error")
	}
	got, _ := io.ReadAll(out.Body)
	if string(got) != body {
		t.Errorf("body not restored on malformed JSON: %q != %q", got, body)
	}
}

func TestJSONBodyRepoIDExtractor_NonStringField(t *testing.T) {
	t.Parallel()
	body := `{"repo_id":42}`
	req := httptest.NewRequest("POST", "/v1/mgmt/rescan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := JSONBodyRepoIDExtractor("repo_id", 1024)
	repoID, _, err := ext(req)
	if err == nil {
		t.Fatalf("non-string repo_id should return error")
	}
	if repoID != "" {
		t.Errorf("repoID should be empty on schema drift, got %q", repoID)
	}
}

func TestJSONBodyRepoIDExtractor_MissingField(t *testing.T) {
	t.Parallel()
	body := `{"other_field":"x"}`
	req := httptest.NewRequest("POST", "/v1/mgmt/rescan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := JSONBodyRepoIDExtractor("repo_id", 1024)
	repoID, _, err := ext(req)
	if err != nil {
		t.Fatalf("missing field is not an error: %v", err)
	}
	if repoID != "" {
		t.Errorf("repoID=%q, want empty", repoID)
	}
}

func TestIsJSONContentType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"Application/JSON", true},
		{"  application/json  ", true},
		{"text/plain", false},
		{"application/x-www-form-urlencoded", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isJSONContentType(c.in); got != c.want {
			t.Errorf("isJSONContentType(%q)=%v, want %v", c.in, got, c.want)
		}
	}
}

// errReader fails immediately on Read; used to validate the
// body-restore path on a failed read.
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, errors.New("boom") }
func (errReader) Close() error               { return nil }

func TestJSONBodyRepoIDExtractor_BodyReadFailureRestoresBody(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("POST", "/v1/mgmt/rescan", strings.NewReader("ignored"))
	req.Header.Set("Content-Type", "application/json")
	req.Body = errReader{}
	ext := JSONBodyRepoIDExtractor("repo_id", 1024)
	_, out, err := ext(req)
	if err == nil {
		t.Fatalf("read failure should surface an extraction error")
	}
	if out == nil || out.Body == nil {
		t.Errorf("body not restored on read failure")
	}
}

// ---------------------------------------------------------------------------
// NestedJSONBodyRepoIDExtractor -- item #3 from iter-2 feedback.
// `mgmt.override` carries repo_id in `scope_filter.repo_id`;
// the gateway must walk that nested path to tag the span.
// ---------------------------------------------------------------------------

func TestNestedJSONBodyRepoIDExtractor_HappyPath(t *testing.T) {
	t.Parallel()
	body := `{"rule_id":"r-1","scope_filter":{"repo_id":"repo-9","scope_kind":"file"},"mute":true,"reason":"ack"}`
	req := httptest.NewRequest("POST", "/v1/mgmt/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := NestedJSONBodyRepoIDExtractor(0, "scope_filter", "repo_id")
	repoID, out, err := ext(req)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if repoID != "repo-9" {
		t.Errorf("repoID=%q, want repo-9", repoID)
	}
	got, _ := io.ReadAll(out.Body)
	if string(got) != body {
		t.Errorf("body not restored intact: %q != %q", got, body)
	}
}

func TestNestedJSONBodyRepoIDExtractor_MissingIntermediate(t *testing.T) {
	t.Parallel()
	body := `{"rule_id":"r-1","mute":true}` // no scope_filter
	req := httptest.NewRequest("POST", "/v1/mgmt/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := NestedJSONBodyRepoIDExtractor(1024, "scope_filter", "repo_id")
	repoID, _, err := ext(req)
	if err != nil {
		t.Fatalf("missing intermediate is not an error: %v", err)
	}
	if repoID != "" {
		t.Errorf("repoID=%q, want empty", repoID)
	}
}

func TestNestedJSONBodyRepoIDExtractor_MissingLeaf(t *testing.T) {
	t.Parallel()
	body := `{"scope_filter":{"scope_kind":"file"}}`
	req := httptest.NewRequest("POST", "/v1/mgmt/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := NestedJSONBodyRepoIDExtractor(1024, "scope_filter", "repo_id")
	repoID, _, err := ext(req)
	if err != nil {
		t.Fatalf("missing leaf is not an error: %v", err)
	}
	if repoID != "" {
		t.Errorf("repoID=%q, want empty", repoID)
	}
}

func TestNestedJSONBodyRepoIDExtractor_NonStringLeaf(t *testing.T) {
	t.Parallel()
	body := `{"scope_filter":{"repo_id":42}}`
	req := httptest.NewRequest("POST", "/v1/mgmt/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := NestedJSONBodyRepoIDExtractor(1024, "scope_filter", "repo_id")
	_, _, err := ext(req)
	if err == nil {
		t.Fatalf("non-string leaf should be an error (schema drift signal)")
	}
}

func TestNestedJSONBodyRepoIDExtractor_NonObjectIntermediate(t *testing.T) {
	t.Parallel()
	body := `{"scope_filter":"oops"}`
	req := httptest.NewRequest("POST", "/v1/mgmt/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := NestedJSONBodyRepoIDExtractor(1024, "scope_filter", "repo_id")
	_, _, err := ext(req)
	if err == nil {
		t.Fatalf("non-object intermediate should be an error")
	}
}

func TestNestedJSONBodyRepoIDExtractor_OversizedBodyRestoredIntact(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 500)
	body := `{"scope_filter":{"repo_id":"repo-7"},"junk":"` + big + `"}`
	req := httptest.NewRequest("POST", "/v1/mgmt/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ext := NestedJSONBodyRepoIDExtractor(64, "scope_filter", "repo_id")
	_, out, err := ext(req)
	if err == nil {
		t.Fatalf("oversized body should signal extraction error")
	}
	got, _ := io.ReadAll(out.Body)
	if string(got) != body {
		t.Errorf("restored body diverged from original (item #4 invariant must hold for nested extractor too)")
	}
}

func TestNestedJSONBodyRepoIDExtractor_EmptyPath_FallsBackToTopLevel(t *testing.T) {
	t.Parallel()
	body := `{"repo_id":"top"}`
	req := httptest.NewRequest("POST", "/v1/mgmt/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Empty path is the documented degenerate case: the
	// extractor falls back to top-level [DefaultRepoIDJSONField].
	// This preserves the invariant that a Wiring composition
	// passing `NestedJSONBodyRepoIDExtractor(0)` accidentally
	// (no path supplied) still produces a usable extractor
	// rather than always-empty.
	ext := NestedJSONBodyRepoIDExtractor(1024)
	repoID, _, err := ext(req)
	if err != nil {
		t.Fatalf("empty path should not error: %v", err)
	}
	if repoID != "top" {
		t.Errorf("empty path should fall back to top-level repo_id, got %q", repoID)
	}
}
