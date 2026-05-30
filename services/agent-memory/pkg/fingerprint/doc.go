// Package fingerprint computes the deterministic 32-byte
// identities the agent-memory service uses to key Nodes and
// Edges in the structural graph (architecture.md G2,
// architecture.md §5.2.1 / §5.2.2).
//
// Two ingests of the same commit MUST produce byte-identical
// fingerprints. The byte encoding of the hash pre-image follows
// the architecture spec exactly (architecture.md §1.3 G2 line
// 45-57; §5.2.1 line 431; §5.2.2 line 449):
//
//	Node fingerprint = sha256( repo_id ‖ kind ‖ canonical_signature ‖ first_seen_sha )
//	Edge fingerprint = sha256( repo_id ‖ kind ‖ src_fingerprint ‖ dst_fingerprint ‖ first_seen_sha )
//
// where `‖` is byte-string concatenation and:
//
//   - `repo_id` is the raw 16-byte UUID (RFC 4122 §4.1.2
//     network-byte-order layout).
//   - `kind` is the UTF-8 encoding of the closed-set discriminator
//     (`method`, `class`, `observed_calls`, etc.). The closed set
//     guarantees no kind value is a prefix of another, so the
//     plain concatenation has no first-segment ambiguity in
//     practice.
//   - `canonical_signature` is the UTF-8 encoding of the
//     language-stable identifier (e.g. `pkg.Foo#bar(int)`).
//   - `src_fingerprint` / `dst_fingerprint` are the raw 32-byte
//     SHA-256 sums of the endpoint Nodes.
//   - `first_seen_sha` is the UTF-8 encoding of the SHA at which
//     the entity first appeared (materialised as the `from_sha`
//     column per §5.2.1 / §5.2.2). Callers MUST pass already-
//     canonicalised values (lowercase hex SHAs, stable kind enum
//     names) — this package does not normalise.
//
// The final fingerprint is the 32-byte SHA-256 sum of that
// concatenation. The encoding is language-agnostic and committed
// to in the package's golden test vectors so a future
// re-implementation in another runtime can reproduce identical
// fingerprints byte-for-byte.
//
// # RepoID derivation from URL (REPO-SCANNER S3.4)
//
// The package also exports RepoIDFromURL(url) which derives a
// deterministic 16-byte RepoID from a repository URL using
// RFC 4122 §4.3 name-based UUIDs (SHA-1 variant, v5) under a
// pinned namespace (`namespaceRepoURL` in repo_id.go):
//
//	RepoID = uuid.NewSHA1(namespaceRepoURL, []byte(url))
//
// This is the helper every graphsink backend uses to assign a
// repo's primary key without coordinating through the database
// (REPO-SCANNER architecture.md §3.4 "AncestryWriter --
// factored from worker.go"). Because the namespace constant
// and the URL are inputs both sides of the wire can compute,
// the Postgres, SQLite, and in-memory adapters all derive the
// same RepoID for the same URL — that byte-equality is what
// makes the S2 / R5 backend-parity claim hold in practice
// (the RepoID enters every NodeFingerprint /
// EdgeFingerprint pre-image, so a divergent RepoID would
// silently shard the node identity space per backend).
//
// Callers MUST treat RepoIDFromURL as a pure function: no
// scheme / case / trailing-slash normalisation is performed.
// Two URL spellings that point at the same physical repo
// (e.g. with and without `.git`) intentionally produce
// distinct RepoIDs; if you need them collapsed, normalise
// before calling. The empty URL is rejected with ErrEmptyURL.
// Golden vectors in `repo_id_test.go` pin the deterministic
// UUIDs for representative URLs (https / ssh-style git /
// file://) so any drift in the namespace, the upstream uuid
// implementation, or the input encoding breaks the test.
package fingerprint
