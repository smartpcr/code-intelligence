// -----------------------------------------------------------------------
// <copyright file="types.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package steward

// PolicyVersion is the canonical shape the rule engine consumes.
type PolicyVersion struct {
	PolicyVersionID string
	Signature       []byte
}

// Rule is one predicate-bearing rule inside a RulePack.
type Rule struct {
	RuleID            string
	PackID            string
	MetricKind        string
	ScopeKind         string
	PredicateDSL      string
	Description       string
	SuggestedRefactor string
}

// RulePack groups related rules under a single pack identity.
type RulePack struct {
	PackID  string
	Version string
}