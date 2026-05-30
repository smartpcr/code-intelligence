package fingerprint

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// namespaceRepoURL is the fixed UUIDv4 that anchors the RFC 4122 §4.3
// name-based UUID derivation used by RepoIDFromURL. It is the single
// namespace constant for the entire `code-intelligence` story: every
// graphsink backend (Postgres, SQLite, in-memory) MUST feed the same
// namespace into uuid.NewSHA1 so that two scans of the same repo URL
// on two backends derive byte-identical RepoIDs (architecture
// REPO-SCANNER S3.4, S2/R5 backend-parity).
//
// The value is a UUIDv4 minted once and pinned here forever. Changing
// it would silently re-key every previously-scanned repo, so the
// pinning is enforced by golden-vector tests in repo_id_test.go that
// hard-code the resulting v5 UUIDs for representative URLs. Any drift
// (deliberate or accidental) breaks those tests.
//
// Why a package variable and not a const? uuid.UUID is a [16]byte
// array type, so uuid.MustParse cannot fold into a Go constant. The
// variable is unexported so callers cannot reassign it.
var namespaceRepoURL = uuid.MustParse("7e9a3d4c-1f5b-4d8e-9a2b-3c4d5e6f7a8b")

// ErrEmptyURL is returned by RepoIDFromURL when called with the
// empty string. The empty URL is rejected (rather than silently
// derived) because every consumer of RepoID — graphsink backends,
// fingerprint pre-images, mgmt-api `repo_id` headers — assumes a
// non-zero RepoID identifies a real repository. Accepting the empty
// URL would mint a stable-but-meaningless RepoID and bleed it across
// unrelated scans (impl-plan Stage 2.1 scenario
// `empty-url-rejected`).
var ErrEmptyURL = errors.New("fingerprint: repo URL must be non-empty")

// RepoIDFromURL derives a deterministic RepoID from a repository URL
// using RFC 4122 §4.3 name-based UUIDs (SHA-1 variant, v5) under the
// pinned namespaceRepoURL.
//
// The function is pure: identical inputs always return byte-identical
// RepoIDs across processes, hosts, and OS architectures. This is what
// lets the Postgres, SQLite, and in-memory graphsink adapters all
// derive the same RepoID for a given URL without coordinating through
// the database — see architecture REPO-SCANNER S3.4 ("Because the
// same RepoIDFromURL(URL) runs in every backend, the RepoID field
// that enters every NodeFingerprint and EdgeFingerprint pre-image is
// byte-identical across backends for the same URL").
//
// Input handling:
//
//   - The URL bytes are fed verbatim to uuid.NewSHA1 as the `name`
//     argument; no normalisation (case-folding, trailing-slash
//     stripping, scheme rewriting, git-URL canonicalisation) is
//     performed. Callers that want to treat `https://github.com/foo/bar`
//     and `https://github.com/foo/bar.git` as the same repo MUST
//     normalise before calling.
//   - The empty string is rejected with ErrEmptyURL and a zero
//     RepoID; see ErrEmptyURL.
//
// Output: a 16-byte RFC 4122 v5 UUID with the version nibble (4 bits
// of byte 6) set to 0101 and the variant nibble (2 bits of byte 8)
// set to 10. The RepoID is the raw 16-byte form; the .String()
// method renders the canonical 8-4-4-4-12 hex grouping.
func RepoIDFromURL(url string) (RepoID, error) {
	if url == "" {
		return RepoID{}, ErrEmptyURL
	}
	u := uuid.NewSHA1(namespaceRepoURL, []byte(url))
	var r RepoID
	copy(r[:], u[:])
	if r.IsZero() {
		return RepoID{}, fmt.Errorf(
			"fingerprint: derived zero RepoID for url %q (impossible under SHA-1)",
			url,
		)
	}
	return r, nil
}
