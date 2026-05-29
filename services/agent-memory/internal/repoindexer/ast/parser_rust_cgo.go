//go:build cgo

package ast

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/rust"
)

// rustParser is a CGO/tree-sitter-backed Rust language parser.
// It requires CGO_ENABLED=1 because the smacker/go-tree-sitter Rust
// binding links against the tree-sitter C library.
type rustParser struct{}

// NewRustParser creates a Rust language parser backed by tree-sitter.
func NewRustParser() Parser { return &rustParser{} }

func (p *rustParser) Language() string     { return "rust" }
func (p *rustParser) Extensions() []string { return []string{".rs"} }

func (p *rustParser) Parse(filename string, src []byte) (ParseResult, error) {
	parser := sitter.NewParser()
	parser.SetLanguage(rust.GetLanguage())
	tree, err := parser.ParseCtx(context.Background(), nil, src)
	if err != nil {
		return ParseResult{}, err
	}
	root := tree.RootNode()

	var pr ParseResult

	// Walk only top-level named children to avoid double-counting
	// nested function_item nodes inside trait/impl bodies.
	for i := 0; i < int(root.NamedChildCount()); i++ {
		child := root.NamedChild(i)
		switch child.Type() {
		case "use_declaration":
			path := tsExtractUsePath(child, src)
			if path != "" {
				pr.Imports = append(pr.Imports, Import{Path: path})
			}

		case "trait_item":
			name := child.ChildByFieldName("name")
			if name != nil {
				traitName := name.Content(src)
				pr.Classes = append(pr.Classes, ClassDecl{Name: traitName})
				body := child.ChildByFieldName("body")
				if body != nil {
					tsExtractTraitMethods(body, traitName, &pr, src)
				}
			}

		case "struct_item":
			name := child.ChildByFieldName("name")
			if name != nil {
				pr.Classes = append(pr.Classes, ClassDecl{Name: name.Content(src)})
			}

		case "impl_item":
			tsProcessImplItem(child, &pr, src)

		case "function_item":
			tsProcessFreeFunction(child, &pr, src)
		}
	}

	return pr, nil
}

// tsExtractUsePath extracts the module path from a use_declaration node.
func tsExtractUsePath(node *sitter.Node, src []byte) string {
	content := node.Content(src)
	content = strings.TrimPrefix(content, "use ")
	content = strings.TrimSuffix(content, ";")
	return strings.TrimSpace(content)
}

// tsExtractTraitMethods extracts method declarations from a trait body.
func tsExtractTraitMethods(body *sitter.Node, traitName string, pr *ParseResult, src []byte) {
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		if child.Type() == "function_item" || child.Type() == "function_signature_item" {
			name := child.ChildByFieldName("name")
			if name != nil {
				pr.Methods = append(pr.Methods, MethodDecl{
					Name:      name.Content(src),
					ClassName: traitName,
				})
			}
		}
	}
}

// tsProcessImplItem extracts methods from an impl block, handling both
// `impl Trait for Struct` (trait impl) and `impl Struct` (inherent impl).
func tsProcessImplItem(node *sitter.Node, pr *ParseResult, src []byte) {
	traitNode := node.ChildByFieldName("trait")
	typeNode := node.ChildByFieldName("type")

	var traitName, structName string
	if traitNode != nil {
		traitName = traitNode.Content(src)
	}
	if typeNode != nil {
		structName = typeNode.Content(src)
	}

	// For `impl Trait for Struct`, mark the struct as implementing the trait.
	if traitName != "" && structName != "" {
		for i, c := range pr.Classes {
			if c.Name == structName {
				if pr.Classes[i].LangMeta == nil {
					pr.Classes[i].LangMeta = make(map[string]string)
				}
				pr.Classes[i].LangMeta["implements"] = traitName
			}
		}
	}

	body := node.ChildByFieldName("body")
	if body == nil {
		return
	}

	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		if child.Type() == "function_item" {
			name := child.ChildByFieldName("name")
			if name != nil {
				md := MethodDecl{
					Name:      name.Content(src),
					ClassName: structName,
				}
				if traitName != "" {
					md.LangMeta = map[string]string{"trait": traitName}
				}
				pr.Methods = append(pr.Methods, md)
			}
		}
	}
}

// tsProcessFreeFunction extracts a top-level free function and any
// method calls (obj.method()) it contains.
func tsProcessFreeFunction(node *sitter.Node, pr *ParseResult, src []byte) {
	name := node.ChildByFieldName("name")
	if name == nil {
		return
	}
	md := MethodDecl{Name: name.Content(src)}

	body := node.ChildByFieldName("body")
	if body != nil {
		var calls []string
		tsFindMethodCalls(body, src, &calls)
		if len(calls) > 0 {
			md.LangMeta = map[string]string{"calls": strings.Join(calls, ",")}
		}
	}

	pr.Methods = append(pr.Methods, md)
}

// tsFindMethodCalls recursively finds call_expression nodes whose function
// is a field_expression (i.e. obj.method() calls, not path calls like
// String::new()).
func tsFindMethodCalls(node *sitter.Node, src []byte, calls *[]string) {
	if node.Type() == "call_expression" {
		fn := node.ChildByFieldName("function")
		if fn != nil && fn.Type() == "field_expression" {
			field := fn.ChildByFieldName("field")
			if field != nil {
				*calls = append(*calls, field.Content(src))
			}
		}
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		tsFindMethodCalls(node.NamedChild(i), src, calls)
	}
}
