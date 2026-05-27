// Package domain defines the core domain types for the clean-code evaluator.
package domain

// Verdict represents the canonical verdict enum for evaluation results.
// The allowed values are exactly: pass, warn, block.
// No other values (e.g., "fail", "gated") are permitted.
type Verdict string

const (
	// VerdictPass indicates all rules passed.
	VerdictPass Verdict = "pass"
	// VerdictWarn indicates at least one rule triggered a warning.
	VerdictWarn Verdict = "warn"
	// VerdictBlock indicates at least one rule triggered a blocking violation.
	VerdictBlock Verdict = "block"
)

// AllVerdicts returns the exhaustive, ordered list of canonical Verdict values.
// This function is the single source of truth for verdict enumeration.
func AllVerdicts() []Verdict {
	return []Verdict{VerdictPass, VerdictWarn, VerdictBlock}
}