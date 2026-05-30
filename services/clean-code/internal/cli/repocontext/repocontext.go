// -----------------------------------------------------------------------
// <copyright file="repocontext.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package repocontext

// RepoContext holds metadata about the repository being analyzed.
type RepoContext struct {
	RootPath string
	RepoID   string
	HeadSHA  string
}