package memory

// SigIndexLenForTest exposes the length of the sigIndex map so
// the e2e test can assert the O(1) fast-path is populated without
// accessing unexported fields. Production code uses
// LookupBySignature; this helper exists solely so the acceptance
// scenario can confirm the index is maintained.
func SigIndexLenForTest(s *Sink) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sigIndex)
}

// NodesLenForTest returns the length of the internal nodes slice.
// Used by the benchmark harness to confirm the fixture size
// without reaching into unexported fields.
func NodesLenForTest(s *Sink) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.nodes)
}
