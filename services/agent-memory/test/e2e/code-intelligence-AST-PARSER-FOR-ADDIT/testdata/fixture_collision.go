package fixture

// fixture_collision.go — Go fixture with both value and pointer receiver
// variants of Bar, plus a sibling function that calls r.Bar().
// This triggers the multimap collision-drop rule (Pass 2b, A5):
// set size > 1 → no static_calls edge, but Bar persists on calls_raw.

type Foo struct{}

func (r Foo) Bar()  {}
func (r *Foo) Bar() {}

func Sibling() {
	r := Foo{}
	r.Bar()
}