package graphreader

import "time"

// RepoSummary is the read-shape of one row from the `repo` table
// projected for the multi-repo overview surfaces: the diagram
// envelope's `repo: { id, url, sha }` block (Stage 7.2) and the
// `GET /v1/repos` list response served by both the management API
// (today inlined in `internal/mgmtapi/read.go:803 handleListRepos`,
// lifted into `internal/graphreader.Reader.ListRepos` in Stage 3.3)
// and the `codeintel serve` HTTP surface (Stage 6).
//
// This struct is the SINGLE SOURCE OF TRUTH for the row shape:
// the three `graphsink.Reader` backends (Postgres, SQLite,
// in-memory) all return `[]graphreader.RepoSummary` directly so
// the wire envelope Stage 7.2 marshals matches byte-for-byte
// regardless of which store backed the scan. Keep field names
// stable: external consumers of the diagram envelope (the
// `services/agent-memory/web/` React + neo4j-nvl client) decode
// the JSON form below into a matching TypeScript type, and any
// rename here forces a coordinated change there.
//
// Field semantics:
//
//   - RepoID is the slash-normalized natural key derived from
//     the repo URL via `pkg/fingerprint.RepoIDFromURL`. It is
//     the identity callers pass back in to `ListNodes` and is
//     stable across Postgres / SQLite / in-memory backends (the
//     backend-parity ID from architecture S3.4).
//
//   - URL is the repo's canonical URL (e.g.
//     `https://github.com/owner/name` or `file:///path/to/repo`
//     for local scans). It is the input to `RepoIDFromURL`.
//
//   - SHA is the commit SHA the row describes (`current_head_sha`
//     on the Postgres `repo` row; the scanned SHA on a CLI
//     single-shot scan). Empty when the row was registered but
//     never indexed.
//
//   - GeneratedAt is the wall-clock time the underlying row was
//     materialised: the `repo.created_at` column on Postgres,
//     the scan timestamp on the SQLite / in-memory backends. It
//     drives the `generatedAt` field on the diagram envelope.
//
//   - RepoUUID is the Postgres UUID surrogate-key (`repo.repo_id`,
//     a `uuid` column). It is populated by the Postgres backend
//     so existing UUID-keyed mgmt-api callers (e.g. `?repo_id=`
//     query filters) keep working; the SQLite / in-memory
//     backends leave it empty because their backend-parity ID
//     lives in `RepoID` and there is no separate surrogate-key
//     namespace. Treat as best-effort metadata, not a primary
//     key.
type RepoSummary struct {
	// RepoID is the backend-parity natural key (see field doc).
	RepoID string
	// URL is the repo's canonical URL.
	URL string
	// SHA is the commit SHA this row describes.
	SHA string
	// GeneratedAt is the row's materialisation wall-clock time.
	GeneratedAt time.Time
	// RepoUUID is the Postgres surrogate-key (best-effort).
	RepoUUID string
}
