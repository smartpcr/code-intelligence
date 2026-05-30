package repoindexer

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// NodeEdgeWriter is the narrow sink interface that AncestryWriter
// writes through. Every graphsink backend (Postgres, SQLite,
// in-memory) implements this interface.
type NodeEdgeWriter interface {
	InsertNode(ctx context.Context, n *Node) error
	InsertEdge(ctx context.Context, e *Edge) error
}

// Node represents a structural element in the code graph.
type Node struct {
	Kind               string
	CanonicalSignature string
	Fingerprint        fingerprint.Sum
}

// Edge represents a relationship between two nodes.
type Edge struct {
	Kind           string
	SrcFingerprint fingerprint.Sum
	DstFingerprint fingerprint.Sum
	Fingerprint    fingerprint.Sum
}