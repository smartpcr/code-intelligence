package api_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestNewOIDCGatewayTrust_LimitedToGatewayComposition is the
// structural enforcement of the iter-6 non-blocking item:
// "keep NewOIDCGatewayTrust usage limited to the OIDC gateway
// composition path by review/policy". Rather than relying on
// review alone, this test walks the service tree and asserts
// that the ONLY production call site for
// [webhook.NewOIDCGatewayTrust] is the api package's
// adapters.go.
//
// # Trust model
//
// `webhook.NewOIDCGatewayTrust()` mints a witness type that
// proves a caller has acknowledged the HMAC-bypass contract
// of `webhook.Router.TrustedGatewayHandler`. The witness is
// type-system enforced at the *function-call* level, but not
// at the package boundary -- any package in the service can
// import `webhook` and call NewOIDCGatewayTrust. This test
// adds the missing package-boundary discipline: a code-review
// reviewer (or a future drift-check pipeline) can run the
// test and see at a glance whether the trust witness has
// spread outside the designated composition root.
//
// # Allowed callers
//
// The allow-list is intentionally narrow:
//
//   - Production code in `services/clean-code/internal/api/`
//     -- that's where the OIDC trust boundary IS, and where
//     the witness is constructed for [Router.TrustedGatewayHandler].
//   - Test files (`*_test.go`) -- tests exercise the gateway
//     path explicitly and SHOULD construct the witness; the
//     test file is by definition not a production runtime
//     path.
//
// Any other package OR any non-test file in another package
// fails this test with a clear error pointing at the offending
// location. A future composition root that wants to mint the
// witness elsewhere MUST update this allow-list AND the
// production trust documentation.
func TestNewOIDCGatewayTrust_LimitedToGatewayComposition(t *testing.T) {
	serviceRoot := findServiceRoot(t)
	allowed := map[string]struct{}{
		// path relative to serviceRoot, normalised to slash
		filepath.ToSlash(filepath.Join("internal", "api", "adapters.go")): {},
	}

	violations := []callSite{}
	err := filepath.Walk(serviceRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			// Skip vendored / generated / cache dirs if any
			// appear in the future. None exist today.
			name := info.Name()
			if name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Tests are explicitly allowed (see doc-comment).
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(serviceRoot, path)
		if err != nil {
			return fmt.Errorf("filepath.Rel(%s, %s): %w", serviceRoot, path, err)
		}
		relSlash := filepath.ToSlash(rel)
		sites := scanFileForCallSites(t, path)
		for _, s := range sites {
			if _, ok := allowed[relSlash]; ok {
				continue
			}
			s.RelPath = relSlash
			violations = append(violations, s)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("filepath.Walk(%s): %v", serviceRoot, err)
	}

	if len(violations) > 0 {
		sort.Slice(violations, func(i, j int) bool {
			if violations[i].RelPath != violations[j].RelPath {
				return violations[i].RelPath < violations[j].RelPath
			}
			return violations[i].Line < violations[j].Line
		})
		msg := strings.Builder{}
		msg.WriteString("webhook.NewOIDCGatewayTrust() was called from outside the allowed call-site set.\n")
		msg.WriteString("Allowed call sites:\n")
		for k := range allowed {
			msg.WriteString("  - " + k + "\n")
		}
		msg.WriteString("Violations:\n")
		for _, v := range violations {
			msg.WriteString(fmt.Sprintf("  - %s:%d (call: %s)\n", v.RelPath, v.Line, v.Snippet))
		}
		msg.WriteString("\n")
		msg.WriteString("The OIDC gateway in internal/api/ is the SOLE production trust boundary.\n")
		msg.WriteString("If you genuinely need to expand the allow-list, update both this test AND the production trust-boundary documentation in internal/api/adapters.go.\n")
		t.Fatal(msg.String())
	}
}

// TestNewOIDCGatewayTrust_AllowedCallSiteExists is the
// positive companion to the negative test above: it verifies
// the allow-list entry actually contains the call. Without
// this assertion an over-broad allow-list could silently
// admit zero call sites and the negative test would still
// pass.
func TestNewOIDCGatewayTrust_AllowedCallSiteExists(t *testing.T) {
	serviceRoot := findServiceRoot(t)
	allowedFile := filepath.Join(serviceRoot, "internal", "api", "adapters.go")
	sites := scanFileForCallSites(t, allowedFile)
	if len(sites) == 0 {
		t.Fatalf("expected webhook.NewOIDCGatewayTrust call in %s but found none -- the trust witness may have been removed without updating the allow-list", allowedFile)
	}
	if len(sites) > 1 {
		t.Logf("note: %s contains %d calls to webhook.NewOIDCGatewayTrust (any non-zero number is acceptable)", allowedFile, len(sites))
	}
}

// callSite captures one source location of a
// `webhook.NewOIDCGatewayTrust` call. Used only for failure
// reporting.
type callSite struct {
	RelPath string
	Line    int
	Snippet string
}

// scanFileForCallSites parses `path` and returns every
// selector-expression call matching
// `<pkgIdent>.NewOIDCGatewayTrust(...)` where `<pkgIdent>` is
// the local name the file uses for the webhook package. The
// scanner resolves the local name from the import block so a
// renamed import (`webhook2 "...webhook"`) is still caught.
//
// Files that fail to parse are reported as a test failure
// (a non-Go file with the .go suffix would be a build
// problem in its own right).
func scanFileForCallSites(t *testing.T, path string) []callSite {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parser.ParseFile(%s): %v", path, err)
	}
	webhookImportPath := "github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook"
	// The default identifier for an unnamed import is the
	// package's actual name ("webhook"). A named import
	// (`name "..."`) overrides this.
	webhookLocalName := ""
	for _, imp := range file.Imports {
		// imp.Path.Value is a quoted string; trim the
		// surrounding quotes for comparison.
		ipath := strings.Trim(imp.Path.Value, `"`)
		if ipath != webhookImportPath {
			continue
		}
		if imp.Name != nil {
			webhookLocalName = imp.Name.Name
		} else {
			webhookLocalName = "webhook"
		}
		break
	}
	if webhookLocalName == "" || webhookLocalName == "_" {
		// The file does not import the webhook package
		// (or imports it for side-effects only) so it
		// cannot make a selector call.
		return nil
	}

	var sites []callSite
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkgIdent.Name != webhookLocalName {
			return true
		}
		if sel.Sel.Name != "NewOIDCGatewayTrust" {
			return true
		}
		pos := fset.Position(call.Pos())
		sites = append(sites, callSite{
			Line:    pos.Line,
			Snippet: fmt.Sprintf("%s.%s(...)", pkgIdent.Name, sel.Sel.Name),
		})
		return true
	})
	return sites
}

// findServiceRoot resolves the absolute path of the
// `services/clean-code` directory by walking up from the
// current test source file. This keeps the test
// location-independent: it works whether `go test` is run
// from the service root, the repo root, or via an editor's
// `go test ./...` invocation.
//
// Fails the test if the directory layout has drifted such
// that the walk-up cannot locate the service root.
func findServiceRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed -- cannot resolve service root")
	}
	// filename is `.../services/clean-code/internal/api/trust_boundary_test.go`.
	// Walk up to `services/clean-code`.
	dir := filepath.Dir(filename)
	for i := 0; i < 16; i++ {
		base := filepath.Base(dir)
		parent := filepath.Dir(dir)
		parentBase := filepath.Base(parent)
		if parentBase == "services" && base == "clean-code" {
			return dir
		}
		if filepath.Dir(parent) == parent {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate services/clean-code root from %s", filename)
	return ""
}
