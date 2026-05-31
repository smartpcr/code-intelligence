//go:build e2e

package postgres

// E2E-only test seam. This file is compiled only under
// `-tags e2e` and exists solely to let the external E2E
// package inject a fake readerBackend into the Reader
// adapter. It is NOT production API.

// ReaderBackendForTest is the E2E-visible shape of the
// unexported readerBackend interface. Any external test
// type satisfying this interface can be passed to
// NewReaderForTest.
type ReaderBackendForTest = readerBackend

// NewReaderForTest wraps newReaderWithBackend for E2E
// tests that need to inject a fake recording backend
// to prove 1:1 call forwarding.
func NewReaderForTest(b ReaderBackendForTest) *Reader {
	return newReaderWithBackend(b)
}
