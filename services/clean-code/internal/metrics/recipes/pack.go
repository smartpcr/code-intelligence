package recipes

// Pack is the closed enum of MetricSample.pack values.
type Pack string

// Source is the closed enum of MetricSample.source values.
type Source string

// Canonical Pack values (architecture Sec 5.2.1).
const (
	PackBase     Pack = "base"
	PackSolid    Pack = "solid"
	PackIngested Pack = "ingested"
	PackSystem   Pack = "system"
)

// Canonical Source values (architecture Sec 5.2.1).
const (
	SourceComputed Source = "computed"
	SourceIngested Source = "ingested"
	SourceDerived  Source = "derived"
)