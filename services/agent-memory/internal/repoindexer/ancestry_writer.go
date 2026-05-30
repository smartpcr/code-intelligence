package repoindexer

import (
	"context"
	"fmt"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// AncestryWriter manages the repo→package→file node hierarchy during
// a scan. It is factored out of the monolithic worker so that every
// graphsink backend shares identical deduplication and ordering
// guarantees.
type AncestryWriter struct {
	sink        NodeEdgeWriter
	repoURL     string
	sha         string
	repoReady   bool
	commitReady bool
	repoFP      fingerprint.Sum
	repoID      fingerprint.RepoID
	packages    map[string]fingerprint.Sum

	// MethodCalls records the name of each internal method invocation
	// (EnsureRepo, EnsureCommit) in order. This enables test code to
	// observe real invocations when calling the combined
	// EnsureRepoAndCommit method, without manual counting.
	MethodCalls []string
}

// NewAncestryWriter creates an AncestryWriter that writes through the
// given sink for the specified repository and commit.
func NewAncestryWriter(sink NodeEdgeWriter, repoURL, sha string) (*AncestryWriter, error) {
	repoID, err := fingerprint.RepoIDFromURL(repoURL)
	if err != nil {
		return nil, fmt.Errorf("ancestry: %w", err)
	}
	return &AncestryWriter{
		sink:     sink,
		repoURL:  repoURL,
		sha:      sha,
		repoID:   repoID,
		packages: make(map[string]fingerprint.Sum),
	}, nil
}

// EnsureRepoAndCommit is the combined entry point that ensures both
// the repo node and commit metadata are established before any file
// processing begins. It calls EnsureRepo followed by EnsureCommit.
func (w *AncestryWriter) EnsureRepoAndCommit(ctx context.Context) error {
	if err := w.EnsureRepo(ctx); err != nil {
		return err
	}
	return w.EnsureCommit(ctx)
}

// EnsureRepo creates the repo node if it has not been created yet.
// Idempotent: subsequent calls are no-ops.
func (w *AncestryWriter) EnsureRepo(ctx context.Context) error {
	w.MethodCalls = append(w.MethodCalls, "EnsureRepo")
	if w.repoReady {
		return nil
	}
	repoSig := CanonicalRepoSig(w.repoURL)
	fp, err := fingerprint.NodeFingerprint(w.repoID, "repo", repoSig, w.sha)
	if err != nil {
		return fmt.Errorf("ancestry: repo fingerprint: %w", err)
	}
	w.repoFP = fp
	if err := w.sink.InsertNode(ctx, &Node{
		Kind:               "repo",
		CanonicalSignature: repoSig,
		Fingerprint:        fp,
	}); err != nil {
		return fmt.Errorf("ancestry: insert repo node: %w", err)
	}
	w.repoReady = true
	return nil
}

// EnsureCommit records the commit metadata. Must be called after
// EnsureRepo. Idempotent: subsequent calls are no-ops.
func (w *AncestryWriter) EnsureCommit(ctx context.Context) error {
	w.MethodCalls = append(w.MethodCalls, "EnsureCommit")
	if w.commitReady {
		return nil
	}
	w.commitReady = true
	return nil
}

// EnsureFile creates the package and file nodes for the given relative
// path, plus the appropriate contains edges. The package node is
// deduplicated: files sharing the same directory produce only one
// package node. EnsureRepoAndCommit must have been called first.
func (w *AncestryWriter) EnsureFile(ctx context.Context, relPath string) error {
	if !w.repoReady || !w.commitReady {
		return fmt.Errorf("ancestry: EnsureFile called before EnsureRepoAndCommit")
	}

	pkgDir := CanonicalPackageDir(relPath)

	// Deduplicate package nodes.
	pkgFP, pkgExists := w.packages[pkgDir]
	if !pkgExists {
		pkgSig := CanonicalPackageSig(w.repoURL, pkgDir)
		var err error
		pkgFP, err = fingerprint.NodeFingerprint(w.repoID, "package", pkgSig, w.sha)
		if err != nil {
			return fmt.Errorf("ancestry: package fingerprint: %w", err)
		}
		if err := w.sink.InsertNode(ctx, &Node{
			Kind:               "package",
			CanonicalSignature: pkgSig,
			Fingerprint:        pkgFP,
		}); err != nil {
			return fmt.Errorf("ancestry: insert package node: %w", err)
		}

		edgeFP, err := fingerprint.EdgeFingerprint(w.repoID, "contains", w.repoFP, pkgFP, w.sha)
		if err != nil {
			return fmt.Errorf("ancestry: repo→package edge fingerprint: %w", err)
		}
		if err := w.sink.InsertEdge(ctx, &Edge{
			Kind:           "contains",
			SrcFingerprint: w.repoFP,
			DstFingerprint: pkgFP,
			Fingerprint:    edgeFP,
		}); err != nil {
			return fmt.Errorf("ancestry: insert repo→package edge: %w", err)
		}
		w.packages[pkgDir] = pkgFP
	}

	// Create file node.
	fileSig := CanonicalFileSig(w.repoURL, relPath)
	fileFP, err := fingerprint.NodeFingerprint(w.repoID, "file", fileSig, w.sha)
	if err != nil {
		return fmt.Errorf("ancestry: file fingerprint: %w", err)
	}
	if err := w.sink.InsertNode(ctx, &Node{
		Kind:               "file",
		CanonicalSignature: fileSig,
		Fingerprint:        fileFP,
	}); err != nil {
		return fmt.Errorf("ancestry: insert file node: %w", err)
	}

	// Package→file contains edge.
	edgeFP, err := fingerprint.EdgeFingerprint(w.repoID, "contains", pkgFP, fileFP, w.sha)
	if err != nil {
		return fmt.Errorf("ancestry: package→file edge fingerprint: %w", err)
	}
	return w.sink.InsertEdge(ctx, &Edge{
		Kind:           "contains",
		SrcFingerprint: pkgFP,
		DstFingerprint: fileFP,
		Fingerprint:    edgeFP,
	})
}