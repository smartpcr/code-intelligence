package mgmtapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// HeadResolver resolves the current HEAD commit SHA of a repo
// at registration / on-demand-ingest time. The Management API
// signature `mgmt.register(repo_url, default_branch)`
// (architecture.md §6.2.1) deliberately does NOT carry a SHA;
// the schema-level `repo.current_head_sha text NOT NULL`
// invariant means *somebody* has to fill the column on insert.
// The resolver is that somebody.
//
// Production composition roots plug in a git-host adapter
// (`git ls-remote`, the GitHub REST API, etc); tests inject a
// fake that returns a fixed value. Splitting the interface
// keeps the handler free of network I/O and trivially testable.
//
// Resolver implementations MUST:
//
//   - Return a lower-case hex SHA (40 or 64 chars; see
//     [IsHexGitSHA]). The handler validates the return value
//     and rejects malformed shapes with a 502 — so an
//     accidentally upper-cased or branch-name return value
//     never lands in the DB.
//   - Return [ErrHeadResolverUnavailable] for transient
//     upstream failures (network timeout, 5xx from the git
//     host). The handler maps this onto a 502 response so the
//     operator can retry.
//   - Return [ErrHeadResolverUnknownRef] when the
//     `defaultBranch` does not exist on the remote. The
//     handler maps this onto a 400 since the operator
//     supplied a bad branch name.
type HeadResolver interface {
	Resolve(ctx context.Context, repoURL, defaultBranch string) (sha string, err error)
}

// ErrHeadResolverUnavailable signals a transient upstream
// failure: the network is unreachable, the git host returned
// 5xx, the JWKS / auth token expired, etc. Handler maps this
// to 502 Bad Gateway.
var ErrHeadResolverUnavailable = errors.New("mgmtapi: head resolver upstream unavailable")

// ErrHeadResolverUnknownRef signals that `defaultBranch` is
// not a valid ref on the remote — operator typo, deleted
// branch, etc. Handler maps this to 400 Bad Request.
var ErrHeadResolverUnknownRef = errors.New("mgmtapi: head resolver: ref not found")

// StaticHeadResolver always returns the configured SHA. Used
// by tests and by the local docker-compose stack where a real
// git remote is not necessarily reachable.
//
// !! DEV / TEST ONLY !! Production deployments MUST plug in a
// real resolver (git ls-remote, GitHub API, etc). The
// AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA env var on the
// cmd/mgmt-api binary configures this fallback explicitly so
// a typo can never silently land a fake SHA in production
// data.
type StaticHeadResolver struct {
	// SHA is the lower-case hex SHA the resolver always
	// returns. MUST satisfy [IsHexGitSHA]; an empty SHA
	// causes Resolve to return [ErrHeadResolverUnavailable]
	// so a misconfigured deployment fails closed.
	SHA string
}

// Resolve implements [HeadResolver]. The repoURL and
// defaultBranch arguments are unused by this stub
// implementation; a real resolver would dispatch on them.
func (r *StaticHeadResolver) Resolve(_ context.Context, _, _ string) (string, error) {
	if !IsHexGitSHA(r.SHA) {
		return "", fmt.Errorf("%w: StaticHeadResolver configured SHA %q is not a valid hex git SHA",
			ErrHeadResolverUnavailable, r.SHA)
	}
	return r.SHA, nil
}

// resolverFunc adapts a plain func into a [HeadResolver]. Used
// internally by the unit tests; not part of the package's
// public surface.
type resolverFunc func(ctx context.Context, repoURL, defaultBranch string) (string, error)

func (f resolverFunc) Resolve(ctx context.Context, u, b string) (string, error) {
	return f(ctx, u, b)
}

// IsHexGitSHA reports whether s is a lower-case hex string of
// a length that a git commit hash can take (40 chars for
// SHA-1, 64 chars for SHA-256). Mirrors the webhookreceiver
// helper of the same shape — kept duplicate-by-design to
// avoid a cross-package dependency on the webhook receiver's
// internal helper. Both copies are tested independently.
//
// Lower-case only: every git host emits SHAs in canonical
// lower-case, and the `ingest_jobs_dedupe_uidx` UNIQUE
// (migration 0006a) treats `ABCD…` and `abcd…` as distinct
// keys. Normalising at the boundary keeps the dedupe
// invariant honest.
func IsHexGitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// normalizeRef canonicalises an operator-supplied branch name
// for the resolver. Trims whitespace, rejects empty values.
// Used by both the request parser and the resolver fakes so
// the handler can rely on a non-empty / non-whitespace string
// at every downstream callsite.
func normalizeRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("default_branch: must not be empty")
	}
	return ref, nil
}
