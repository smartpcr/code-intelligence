package repoindexer

import (
	"path"
	"strings"
)

// CanonicalRepoSig returns the canonical signature for a repo node.
func CanonicalRepoSig(repoURL string) string {
	return repoURL
}

// CanonicalPackageDir extracts the slash-normalized directory part
// of relPath. Returns "" for files at the repo root.
func CanonicalPackageDir(relPath string) string {
	normalized := strings.ReplaceAll(relPath, "\\", "/")
	dir := path.Dir(normalized)
	if dir == "." {
		return ""
	}
	return dir
}

// CanonicalPackageSig returns the canonical signature for a package node.
func CanonicalPackageSig(repoURL, pkgDir string) string {
	return repoURL + "::pkg::" + pkgDir
}

// CanonicalFileSig returns the canonical signature for a file node.
func CanonicalFileSig(repoURL, relPath string) string {
	return repoURL + "::file::" + relPath
}