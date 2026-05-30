package fingerprint

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// namespaceRepoURL is the fixed UUIDv4 that anchors the RFC 4122 §4.3
// name-based UUID derivation used by RepoIDFromURL.
var namespaceRepoURL = uuid.MustParse("7e9a3d4c-1f5b-4d8e-9a2b-3c4d5e6f7a8b")

// ErrEmptyURL is returned by RepoIDFromURL when called with the
// empty string.
var ErrEmptyURL = errors.New("fingerprint: repo URL must be non-empty")

// RepoIDFromURL derives a deterministic RepoID from a repository URL
// using RFC 4122 §4.3 name-based UUIDs (SHA-1 variant, v5).
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