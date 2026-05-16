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
package fingerprint
