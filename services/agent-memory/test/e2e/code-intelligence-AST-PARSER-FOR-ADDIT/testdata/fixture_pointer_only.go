package fixture

// fixture_pointer_only.go — Go fixture with only the pointer-receiver
// method for Bar, plus a sibling that calls r.Bar().
// With only one target in the multimap (set size == 1), the emitter
// resolves the static_calls edge to *Foo.Bar.

type Foo struct{}

func (r *Foo) Bar() {}

func Sibling() {
	r := &Foo{}
	r.Bar()
}