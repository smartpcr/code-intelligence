package repoindexer

import (
	"context"
	"log/slog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ASTEmitter is the per-language parser hook the Stage 3.2
// dispatcher implements. The Stage 3.1 worker delegates one
// EmitFile call per File Node it ensures during a full ingest;
// the emitter is responsible for opening the file via
// `ev.Open`, parsing it with the appropriate tree-sitter
// grammar, and writing the resulting Class / Method / Block
// Nodes plus static edges through its own GraphWriter handle.
//
// Stage 3.1 ships only the no-op default implementation
// (`NoopASTEmitter`) so the polling loop and ancestry writer
// compile without the tree-sitter dependency surface; Stage 3.2
// will swap in the real dispatcher without touching the worker.
type ASTEmitter interface {
	// EmitFile is invoked once per File Node the worker ensures.
	// Returning a non-nil error marks the ingest as failed and
	// the worker records `status='failed'` on the ingest_jobs
	// row -- callers should reserve errors for true unrecoverable
	// failures (parser crashes, IO errors). Parser warnings or
	// per-file syntactic noise should be logged and swallowed so
	// one malformed file does not abort the whole ingest.
	EmitFile(ctx context.Context, ev EmitFileEvent) error
}

// EmitFileEvent is the payload the worker hands to the AST
// emitter. The fields name everything the emitter needs to
// stitch its produced Nodes/Edges into the existing ancestry
// without re-reading the queue row.
type EmitFileEvent struct {
	// RepoID is the canonical 16-byte form of the repo's UUID.
	// Required by `graphwriter.NodeInput.RepoID` -- the emitter
	// passes it through unchanged so derived Nodes share the
	// parent repo.
	RepoID fingerprint.RepoID
	// RepoURL is the repository's natural URL (the `repo.url`
	// column). Emitters use it to derive canonical signatures
	// for Class / Method / Block nodes that need to remain
	// stable across re-ingests of the same SHA.
	RepoURL string
	// SHA is the commit SHA being ingested. Mirrors
	// `Node.from_sha`.
	SHA string
	// FileNodeID is the textual UUID of the File Node the
	// worker has already ensured. Emitter-derived Class
	// Nodes set `parent_node_id = FileNodeID` so the
	// repo→package→file→class chain stays intact.
	FileNodeID string
	// RelPath is the workspace-relative path of the file,
	// always forward-slash. Useful for the emitter's log
	// records and for deriving the language hint.
	RelPath string
	// AbsPath is the absolute filesystem path of the file. Empty
	// for in-memory workspaces; emitters MUST prefer `Open`.
	AbsPath string
	// Open returns a fresh ReadCloser for the file's contents.
	// Each call returns a new reader at offset 0 so the emitter
	// can perform multiple passes (e.g. lex + parse) without
	// rewinding.
	Open func() (ReadCloser, error)
}

// NoopASTEmitter is the Stage 3.1 default ASTEmitter. It does
// no parsing and emits no Nodes/Edges -- its single job is to
// keep the worker dispatcher compiling while Stage 3.2 lands
// the real tree-sitter dispatcher.
//
// The package-level `slog.Default()` logger is used at debug
// level so production deployments see "files were walked but
// the AST emitter is a no-op" in their structured logs,
// surfacing a misconfigured wiring before it silently hides
// missed Method/Block ingest. Override with `WithLogger`.
type NoopASTEmitter struct {
	Logger *slog.Logger
}

// EmitFile is the no-op implementation. It emits a single
// debug-level structured log record per file so operators
// can see the dispatcher is wired but not yet emitting --
// the line disappears once Stage 3.2 swaps in the real
// dispatcher.
func (n NoopASTEmitter) EmitFile(_ context.Context, ev EmitFileEvent) error {
	logger := n.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("repoindexer.ast.noop",
		slog.String("op", "emit_file"),
		slog.String("rel_path", ev.RelPath),
		slog.String("file_node_id", ev.FileNodeID),
		slog.String("sha", ev.SHA),
	)
	return nil
}
