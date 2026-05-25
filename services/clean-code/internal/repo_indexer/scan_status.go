package repo_indexer

import (
	"errors"
	"fmt"
)

// ScanStatus represents the canonical lifecycle states of a commit scan.
// The closed set is: pending -> scanning -> scanned | failed.
//
// ScanStatus has a string underlying type so the canonical wire literals
// ("pending", "scanning", "scanned", "failed") are also valid SQL column
// values for `commit.scan_status` and so callers can construct an
// arbitrary candidate via ScanStatus("...") for validation without going
// through a numeric round-trip.
type ScanStatus string

const (
	// ScanStatusPending is the initial state when a commit is registered.
	ScanStatusPending ScanStatus = "pending"
	// ScanStatusScanning indicates the commit is actively being scanned.
	ScanStatusScanning ScanStatus = "scanning"
	// ScanStatusScanned indicates the scan completed successfully.
	ScanStatusScanned ScanStatus = "scanned"
	// ScanStatusFailed indicates the scan terminated with an error.
	ScanStatusFailed ScanStatus = "failed"
)

// canonicalScanStatuses pins membership of the closed set. Lookup is the
// single source of truth for Validate / String / *Transition.
var canonicalScanStatuses = map[ScanStatus]struct{}{
	ScanStatusPending:  {},
	ScanStatusScanning: {},
	ScanStatusScanned:  {},
	ScanStatusFailed:   {},
}

// String returns the canonical lowercase name of the ScanStatus. For a
// non-canonical value it returns a debug-friendly wrapper that quotes the
// underlying literal so it is unambiguous in logs.
func (s ScanStatus) String() string {
	if _, ok := canonicalScanStatuses[s]; ok {
		return string(s)
	}
	return fmt.Sprintf("ScanStatus(%q)", string(s))
}

// AllScanStatuses returns every valid ScanStatus value in canonical
// (lifecycle) order. Each call returns a fresh slice so a caller mutating
// the returned slice cannot leak back into package state.
func AllScanStatuses() []ScanStatus {
	return []ScanStatus{
		ScanStatusPending,
		ScanStatusScanning,
		ScanStatusScanned,
		ScanStatusFailed,
	}
}

// Validate returns ErrInvalidScanStatus if s is not a member of the
// canonical set, and nil otherwise.
func (s ScanStatus) Validate() error {
	if _, ok := canonicalScanStatuses[s]; !ok {
		return ErrInvalidScanStatus
	}
	return nil
}

// IsTerminal returns true for scanned and failed -- states that cannot
// transition further on the canonical diagram.
func (s ScanStatus) IsTerminal() bool {
	return s == ScanStatusScanned || s == ScanStatusFailed
}

// CanTransition reports whether (from, to) is a permitted edge in the
// canonical lifecycle diagram:
//
//	pending  -> scanning
//	scanning -> scanned
//	scanning -> failed
//
// All other pairs return false. In particular:
//   - terminal-out (scanned/failed -> anything) is rejected, because a
//     "rescan" that mutates a scanned row would orphan the previous
//     metric_sample set.
//   - self-edges (X -> X) are rejected, because an UPDATE that does not
//     change the value is a writer bug, not a no-op.
//   - pending -> terminal is rejected, because it drops the in-flight
//     observability window the `scanning` state guarantees.
//
// CanTransition assumes both arguments are canonical; callers that
// receive untrusted values should use ValidateTransition instead so a
// non-member surfaces as ErrInvalidScanStatus rather than being silently
// classified as an off-diagram edge.
func CanTransition(from, to ScanStatus) bool {
	switch from {
	case ScanStatusPending:
		return to == ScanStatusScanning
	case ScanStatusScanning:
		return to == ScanStatusScanned || to == ScanStatusFailed
	}
	return false
}

// ValidateTransition validates an attempted state change end-to-end.
//
// It first confirms that both `from` and `to` are members of the
// canonical set; if either is unknown, it returns ErrInvalidScanStatus.
// If both are canonical but the edge is not on the diagram (see
// CanTransition for the diagram), it returns
// ErrInvalidScanStatusTransition. Canonical-and-on-diagram edges return
// nil.
//
// The two-error split lets writers distinguish "the value is junk" from
// "the value is fine, but the move is illegal".
func ValidateTransition(from, to ScanStatus) error {
	if err := from.Validate(); err != nil {
		return err
	}
	if err := to.Validate(); err != nil {
		return err
	}
	if !CanTransition(from, to) {
		return ErrInvalidScanStatusTransition
	}
	return nil
}

// Sentinel errors for callers to match on with errors.Is.
var (
	ErrInvalidScanStatus           = errors.New("invalid scan status")
	ErrInvalidScanStatusTransition = errors.New("invalid scan status transition")
)
