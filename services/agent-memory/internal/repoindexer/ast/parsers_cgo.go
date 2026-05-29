package ast

func init() {
	RegisterParser(NewTreeSitterCSharpParser())
}
