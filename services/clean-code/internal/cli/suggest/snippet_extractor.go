// -----------------------------------------------------------------------
// <copyright file="snippet_extractor.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package suggest

import (
	"path/filepath"
)

// FileSnippetExtractor returns a [SnippetExtractor] that
// resolves the snippet by reading bytes from disk via
// [ExtractSnippet]. `repoRoot` is the absolute filesystem path
// of the repo under analysis; `scope.FilePath` is treated as
// repo-relative and is joined with `repoRoot` to form the
// absolute path passed to [ExtractSnippet]. `maxLines` is the
// snippet cap (callers SHOULD pass [DefaultSnippetMaxLines]
// unless the operator overrode via the reserved
// `--snippet-cap-lines` flag).
//
// When `scope.FilePath` is already absolute, the join leaves
// it unchanged (per `filepath.Join` semantics on most
// platforms; on Windows an absolute Unix-style path is
// treated as rooted at the current drive -- callers that
// support cross-platform absolute paths SHOULD pre-resolve).
//
// The returned extractor is safe for concurrent use because
// [ExtractSnippet] opens and reads the file on each call
// without sharing state.
func FileSnippetExtractor(repoRoot string, maxLines int) SnippetExtractor {
	return func(scope Scope) (string, bool, error) {
		abs := scope.FilePath
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(repoRoot, scope.FilePath)
		}
		return ExtractSnippet(abs, scope.StartLine, scope.EndLine, maxLines)
	}
}
