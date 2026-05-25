package repo_indexer

import (
	"errors"
	"fmt"
)

// ScanStatus represents the canonical lifecycle states of a commit scan.
// The closed set is: pending → scanning → scanned | failed.
type ScanStatus int

const (
	// ScanStatusPending is the initial state when a commit is registered.
	ScanStatusPending ScanStatus = iota
	// ScanStatusScanning indicates the commit is actively being scanned.
	ScanStatusScanning
	// ScanStatusScanned indicates the scan completed successfully.
	ScanStatusScanned
	// ScanStatusFailed indicates the scan terminated with an error.
	ScanStatusFailed
)

// scanStatusNames maps each ScanStatus constant to its canonical string.
var scanStatusNames = map[ScanStatus]string{
	ScanStatusPending:  "pending",
	ScanStatusScanning: "scanning",
	ScanStatusScanned:  "scanned",
	ScanStatusFailed:   "failed",
}

// String returns the canonical lowercase name of the ScanStatus.
func (s ScanStatus) String() string {
	if name, ok := scanStatusNames[s]; ok {
		return name
	}
	return fmt.Sprintf("ScanStatus(%d)", int(s))
}

// AllScanStatuses returns every valid ScanStatus value in ordinal order.
func AllScanStatuses() []ScanStatus {
	return []ScanStatus{
		ScanStatusPending,
		ScanStatusScanning,
		ScanStatusScanned,
		ScanStatusFailed,
	}
}

// Validate returns an error if s is not a member of the canonical set.
func (s ScanStatus) Validate() error {
	if _, ok := scanStatusNames[s]; !ok {
		return ErrInvalidScanStatus
	}
	return nil
}

// IsTerminal returns true for scanned and failed — states that cannot
// transition further.
func (s ScanStatus) IsTerminal() bool {
	return s == ScanStatusScanned || s == ScanStatusFailed
}

// ErrInvalidScanStatus is returned by Validate when a ScanStatus value is
// not a member of the canonical set. Callers can match on it with errors.Is.
var ErrInvalidScanStatus = errors.New("invalid scan status")
