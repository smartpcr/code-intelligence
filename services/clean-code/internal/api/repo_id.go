package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// RepoIDExtractor pulls a canonical `repo_id` value off an
// authenticated request so the gateway can stamp it on the
// OTel span (architecture Sec 8 / impl-plan Stage 6.4 require
// the span to carry `verb`, `caller_subject`, and `repo_id`).
//
// Returns the extracted repo_id, the request to forward
// downstream (a body-peeking extractor MUST restore the
// body), and a non-fatal extraction error. A non-nil error
// is logged but does NOT fail the request -- the downstream
// verb handler is the authoritative body validator.
//
// Returning `nil` for the request means "the gateway can
// keep using the original request" -- typical for header /
// query extractors that do not touch the body.
//
// The architecture's verbs read repo_id from one of four
// places (per the wire-shape definitions in
// internal/management, internal/policy/steward, and
// internal/ingest/webhook):
//
//   - Top-level JSON body field `repo_id`
//     (`mgmt.register_repo`, `mgmt.rescan`, `eval.gate`,
//     `ingest.churn`, `ingest.defects`, ...). Use
//     [JSONBodyRepoIDExtractor].
//   - Nested JSON body field (e.g. `mgmt.override` carries
//     `scope_filter.repo_id` per
//     `internal/policy/steward/types.go`). Use
//     [NestedJSONBodyRepoIDExtractor].
//   - URL query parameter `repo_id` (`mgmt.read.*`
//     idempotent reads). Use [QueryRepoIDExtractor].
//   - HTTP request header (`X-Forge-Repo-ID` for the
//     header-borne ingest shapes such as
//     `ingest.coverage` / `ingest.test_balance`). Use
//     [HeaderRepoIDExtractor].
//   - Not present (`policy.activate` carries
//     `policy_version_id` instead, etc.). Use
//     [NoRepoIDExtractor].
type RepoIDExtractor func(r *http.Request) (string, *http.Request, error)

// DefaultRepoIDJSONField is the canonical JSON field name
// every top-level wire shape uses for the repo identifier.
// Pinned as a constant so the default extractor is shared
// across every verb that lands a request body.
const DefaultRepoIDJSONField = "repo_id"

// DefaultRepoIDQueryParam is the canonical query parameter
// name the read-side verbs use (`?repo_id=...`).
const DefaultRepoIDQueryParam = "repo_id"

// DefaultRepoIDPeekBytes caps how many bytes the JSON body
// extractor BUFFERS to find the `repo_id` field. NOTE: this
// is the peek window only -- the extractor PRESERVES the
// full body downstream by chaining the buffered bytes with
// the unread tail of the original body (see
// [JSONBodyRepoIDExtractor]'s body-restore contract). The
// 8 KiB default covers every canonical wire shape in
// `internal/management` and `internal/ingest/webhook` while
// staying small enough that a malicious caller cannot force
// the gateway to buffer a multi-megabyte body just for span
// attribution.
const DefaultRepoIDPeekBytes int64 = 8 << 10 // 8 KiB

// NoRepoIDExtractor is the zero-cost extractor for verbs that
// do not carry a repo_id (`policy.activate`,
// `policy.keys.list_active`, ...). The span attribute is
// set to "".
func NoRepoIDExtractor(r *http.Request) (string, *http.Request, error) {
	return "", r, nil
}

// HeaderRepoIDExtractor returns an extractor that reads
// repo_id from the named HTTP request header. Used by the
// header-borne wire shapes (`X-Forge-Repo-ID` in
// `internal/ingest/webhook` for `ingest.coverage` /
// `ingest.test_balance`). The header value is returned
// verbatim; empty header is treated as "no repo_id" (returns
// empty string + nil error).
func HeaderRepoIDExtractor(headerName string) RepoIDExtractor {
	return func(r *http.Request) (string, *http.Request, error) {
		return r.Header.Get(headerName), r, nil
	}
}

// QueryRepoIDExtractor returns an extractor that reads
// repo_id from the named URL query parameter. Used by the
// read-side verbs (mgmt.read.* — they live under GET with
// query parameters). Empty / missing parameter returns empty
// string + nil error.
func QueryRepoIDExtractor(paramName string) RepoIDExtractor {
	return func(r *http.Request) (string, *http.Request, error) {
		if r.URL == nil {
			return "", r, nil
		}
		return r.URL.Query().Get(paramName), r, nil
	}
}

// JSONBodyRepoIDExtractor returns an extractor that peeks the
// request body for a top-level JSON `fieldName` value, then
// restores the body so the downstream handler reads it
// intact. The extractor BUFFERS at most `maxBytes` bytes for
// the peek; bodies larger than `maxBytes` cause an
// extraction error (the field is NOT extracted, but the
// body is restored INTACT -- including the part beyond the
// peek window -- so the downstream handler still sees the
// complete payload).
//
// # Body-restore contract (item #4 iter-2 evaluator fix)
//
// The peek window is decoupled from the request size. The
// extractor:
//
//  1. Reads at most `maxBytes+1` bytes from the body into a
//     buffer (so it can detect oversize).
//  2. Restores the body as
//     `io.MultiReader(bytes.NewReader(buf), origBody)` so
//     the downstream handler reads BOTH the buffered prefix
//     AND any unread tail of the original body -- a body
//     that exceeds `maxBytes` is NOT corrupted.
//  3. JSON-unmarshals the buffered prefix to find
//     `fieldName`. A buffered prefix that is NOT a complete
//     JSON document is an oversize-skip (the body is
//     restored intact but no repo_id is extracted).
//
// `fieldName` empty defaults to [DefaultRepoIDJSONField].
// `maxBytes` <= 0 defaults to [DefaultRepoIDPeekBytes].
//
// Only `application/json` bodies are inspected; other
// content types return (empty, r, nil) so the gateway does
// not buffer multipart uploads.
func JSONBodyRepoIDExtractor(fieldName string, maxBytes int64) RepoIDExtractor {
	if fieldName == "" {
		fieldName = DefaultRepoIDJSONField
	}
	if maxBytes <= 0 {
		maxBytes = DefaultRepoIDPeekBytes
	}
	return func(r *http.Request) (string, *http.Request, error) {
		buf, oversize, restored, err := peekJSONBody(r, maxBytes)
		if err != nil {
			return "", restored, err
		}
		if buf == nil {
			// Non-JSON / empty body / no body -- skip.
			return "", restored, nil
		}
		if oversize {
			// Body is too large for the peek window;
			// surface the extraction error but the
			// body is restored INTACT for the
			// downstream handler.
			return "", restored, fmt.Errorf("api: body larger than %d-byte repo_id peek window", maxBytes)
		}
		val, found, err := extractJSONField(buf, fieldName)
		if err != nil {
			return "", restored, err
		}
		if !found {
			return "", restored, nil
		}
		return val, restored, nil
	}
}

// NestedJSONBodyRepoIDExtractor returns an extractor that
// walks a NESTED JSON object path before pulling the
// `repo_id` field. Used by verbs whose canonical wire shape
// places `repo_id` inside a sub-object -- most notably
// `mgmt.override`, whose request body is
// `{rule_id, scope_filter: {repo_id, scope_kind,
// scope_signature_glob}, mute, reason}` per
// `internal/policy/steward/types.go`.
//
// `path` is the sequence of JSON object keys to traverse
// before reading the leaf string. An empty path is
// equivalent to [JSONBodyRepoIDExtractor] with the default
// field name. `maxBytes` defaults to [DefaultRepoIDPeekBytes]
// when <= 0.
//
// The same body-restore contract as [JSONBodyRepoIDExtractor]
// applies: the request body is restored INTACT (including
// any unread tail beyond the peek window) before the
// extractor returns.
func NestedJSONBodyRepoIDExtractor(maxBytes int64, path ...string) RepoIDExtractor {
	if maxBytes <= 0 {
		maxBytes = DefaultRepoIDPeekBytes
	}
	clonedPath := append([]string(nil), path...)
	return func(r *http.Request) (string, *http.Request, error) {
		buf, oversize, restored, err := peekJSONBody(r, maxBytes)
		if err != nil {
			return "", restored, err
		}
		if buf == nil {
			return "", restored, nil
		}
		if oversize {
			return "", restored, fmt.Errorf("api: body larger than %d-byte repo_id peek window", maxBytes)
		}
		val, found, err := extractNestedJSONField(buf, clonedPath)
		if err != nil {
			return "", restored, err
		}
		if !found {
			return "", restored, nil
		}
		return val, restored, nil
	}
}

// peekJSONBody reads up to `maxBytes+1` bytes from `r.Body`,
// then restores the body so the downstream handler reads the
// complete payload (buffered prefix + unread tail). Returns
// the buffered bytes, an `oversize` flag (true when the
// buffered prefix consumed maxBytes+1 bytes, meaning more
// remains in the body), the request with the restored body,
// and an error iff body reading failed (which is itself a
// non-fatal extraction signal).
//
// When `r.Body == nil`, `r.Body == http.NoBody`, or the
// Content-Type is not JSON, returns (nil, false, r, nil) --
// the caller treats this as "no extraction possible, no
// error".
func peekJSONBody(r *http.Request, maxBytes int64) ([]byte, bool, *http.Request, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, false, r, nil
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		return nil, false, r, nil
	}
	orig := r.Body
	limited := io.LimitReader(orig, maxBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		// Body read failed; restore whatever we read so
		// the downstream handler observes the same
		// truncation and surfaces a precise error.
		r.Body = io.NopCloser(bytes.NewReader(buf))
		return nil, false, r, fmt.Errorf("api: reading body for repo_id extract: %w", err)
	}
	oversize := int64(len(buf)) > maxBytes
	// Restore the body. For a NON-oversized body, the
	// buffered bytes ARE the whole body, and the original
	// reader is at EOF -- the MultiReader is fine. For an
	// OVERSIZED body, MultiReader chains the buffered
	// prefix with the unread tail of the original body so
	// downstream sees the complete payload. The original
	// io.Closer is preserved via `chainedBody.Close`.
	r.Body = &chainedBody{
		reader: io.MultiReader(bytes.NewReader(buf), orig),
		closer: orig,
	}
	if oversize {
		return buf, true, r, nil
	}
	return buf, false, r, nil
}

// chainedBody adapts the (Reader, Closer) pair returned by
// [peekJSONBody] into an [io.ReadCloser] that delegates Read
// to the chained MultiReader and Close to the ORIGINAL body
// closer.  The split is necessary because
// `io.NopCloser(io.MultiReader(...))` would leak the
// original body (no Close ever runs); doing the wrap
// manually keeps the resource-management contract correct.
type chainedBody struct {
	reader io.Reader
	closer io.Closer
}

func (c *chainedBody) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *chainedBody) Close() error               { return c.closer.Close() }

// extractJSONField decodes `buf` and returns the value of
// the top-level string field `fieldName`. `found=false`
// (with err==nil) means the field is absent. A non-string
// value, malformed JSON, or any other decode failure
// returns a non-fatal error.
func extractJSONField(buf []byte, fieldName string) (string, bool, error) {
	if len(buf) == 0 {
		return "", false, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(buf, &probe); err != nil {
		return "", false, fmt.Errorf("api: decoding body for repo_id extract: %w", err)
	}
	raw, ok := probe[fieldName]
	if !ok {
		return "", false, nil
	}
	var val string
	if err := json.Unmarshal(raw, &val); err != nil {
		return "", false, fmt.Errorf("api: %s field is not a JSON string: %w", fieldName, err)
	}
	return val, true, nil
}

// extractNestedJSONField walks `path` through nested JSON
// objects and returns the leaf string at
// `path[len(path)-1]`. `path` empty falls back to the
// default top-level repo_id field. `found=false` means a
// step along the path is missing OR the leaf is missing --
// neither is an error (it just means the verb's request
// happens to omit repo_id).
func extractNestedJSONField(buf []byte, path []string) (string, bool, error) {
	if len(path) == 0 {
		return extractJSONField(buf, DefaultRepoIDJSONField)
	}
	if len(buf) == 0 {
		return "", false, nil
	}
	current := json.RawMessage(buf)
	for i, key := range path {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(current, &obj); err != nil {
			return "", false, fmt.Errorf("api: decoding nested body for repo_id extract at path[%d]=%q: %w", i, key, err)
		}
		next, ok := obj[key]
		if !ok {
			return "", false, nil
		}
		current = next
	}
	var val string
	if err := json.Unmarshal(current, &val); err != nil {
		return "", false, fmt.Errorf("api: nested field %v is not a JSON string: %w", path, err)
	}
	return val, true, nil
}

// isJSONContentType returns true iff `contentType` advertises
// JSON. Accepts `application/json` and `application/json;
// charset=utf-8` (the two shapes the existing verb handlers
// in this repository emit). Case-insensitive on the type /
// subtype per RFC 7231.
func isJSONContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	// Take everything up to the first semicolon, trim,
	// lowercase compare.
	for i := 0; i < len(contentType); i++ {
		if contentType[i] == ';' {
			contentType = contentType[:i]
			break
		}
	}
	for i := 0; i < len(contentType); i++ {
		c := contentType[i]
		if c >= 'A' && c <= 'Z' {
			contentType = contentType[:i] + string(c+32) + contentType[i+1:]
		}
	}
	for len(contentType) > 0 && contentType[0] == ' ' {
		contentType = contentType[1:]
	}
	for len(contentType) > 0 && contentType[len(contentType)-1] == ' ' {
		contentType = contentType[:len(contentType)-1]
	}
	return contentType == "application/json"
}
