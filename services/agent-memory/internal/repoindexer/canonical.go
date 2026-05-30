package repoindexer

import "path"

// canonical.go hosts the four canonical-signature helpers used to
// derive deterministic `node.canonical_signature` values for the
// repo→package→file portion of the structural graph.
//
// The helpers were originally unexported and lived alongside the
// full-ingest implementation in worker.go. They are promoted here
// (under exported names) so packages outside repoindexer -- in
// particular the upcoming graphsink backends (`internal/graphsink/
// sqlite`, `internal/graphsink/memory`) and the diagram projector
// (`internal/diagram`) -- can mint identities that match the
// production Postgres write path byte-for-byte. The CLI-side
// SQLite scan and the production worker MUST emit the same
// `(repo_id, kind, canonical_signature)` tuple for the same input
// or a follow-on Postgres re-scan would split node identity.
//
// Every change to a function in this file is a wire-compatibility
// change: it shifts the canonical_signature of every node minted
// by every future scan. The accompanying canonical_test.go pins
// the exact byte output to make accidental drift a hard test
// failure rather than a silent re-identification of the graph.

// CanonicalRepoSig is the canonical signature for the root Repo
// Node. Just the URL -- there's only one Repo Node per repo so a
// richer signature would be redundant.
func CanonicalRepoSig(repoURL string) string { return repoURL }

// CanonicalPackageDir normalises the directory key the
// package cache uses. Returns "" for files at the repo root (so
// the root "package" has a stable signature) and the
// forward-slash directory path otherwise.
//
// path.Dir from Go's standard library returns "." for files
// without a directory; we collapse that to "" so the canonical
// signature reads as `<url>::pkg::` rather than `<url>::pkg::.`,
// matching how operators expect to read the value.
func CanonicalPackageDir(relPath string) string {
	d := path.Dir(relPath)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// CanonicalPackageSig is the canonical signature for a Package
// Node. The format is `<repo url>::pkg::<dir path>` where the
// dir path is forward-slash relative. Choosing a distinct
// `::pkg::` separator prevents collisions with the file-level
// canonical signature `::file::<path>` -- a directory named
// `foo.go` cannot collide with a file named `foo.go` because
// the segment between `<repo url>` and the path differs.
func CanonicalPackageSig(repoURL, dir string) string {
	return repoURL + "::pkg::" + dir
}

// CanonicalFileSig is the canonical signature for a File Node.
// Format `<repo url>::file::<rel path>`.
func CanonicalFileSig(repoURL, relPath string) string {
	return repoURL + "::file::" + relPath
}
