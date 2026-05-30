// -----------------------------------------------------------------------
// <copyright file="devpolicy.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package devpolicy

// DevPolicyLoader loads YAML rule packs into an unsigned in-memory
// PolicyVersion for local development use.
type DevPolicyLoader struct {
	RulePackPaths []string
}