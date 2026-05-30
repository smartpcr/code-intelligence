// -----------------------------------------------------------------------
// <copyright file="scopebinding.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package scopebinding

// ScopeBinding associates a scope identifier with its source location.
type ScopeBinding struct {
	ScopeID   string
	ScopeKind string
	FilePath  string
	StartLine int
	EndLine   int
}