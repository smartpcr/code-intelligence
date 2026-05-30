// Polyglot smoke fixture: Go.
// Must declare a class/type, a free function, a same-file call,
// and one import so the dispatcher emits >=1 class node,
// >=1 method node, and >=1 static_calls edge per language.
// The struct counts as the type; the same-file call flows
// from greet -> formatGreeting (both package-level free funcs).
package hello

import "fmt"

type Greeter struct {
	prefix string
}

func formatGreeting(name string) string {
	return fmt.Sprintf("hi %s", name)
}

func greet(name string) string {
	return formatGreeting(name)
}
