package index

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/omorsi45/hazard/internal/graph"
)

const (
	maxAsserts     = 5
	maxAssertRunes = 80
)

// discoverGo finds Go tests statically in the test files of the graph's Go
// packages. A test file that cannot be read or parsed fails discovery: an
// unreadable file must not silently become an empty inventory.
func discoverGo(repoDir string, g *graph.Graph) ([]Test, error) {
	ids := make([]string, 0, len(g.Packages))
	for id, p := range g.Packages {
		if p.Language == "go" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var tests []Test
	for _, id := range ids {
		for _, rel := range g.Packages[id].TestFiles {
			found, err := discoverGoFile(repoDir, id, rel)
			if err != nil {
				return nil, err
			}
			tests = append(tests, found...)
		}
	}
	return tests, nil
}

func discoverGoFile(repoDir, importPath, rel string) ([]Test, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoDir, filepath.FromSlash(rel)), nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("discover Go tests in %s: %w", rel, err)
	}
	testingNames := testingImportNames(file)
	var tests []Test
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil || fd.Type.TypeParams != nil || fd.Type.Results != nil {
			continue
		}
		kind, ok := testKind(fd.Name.Name)
		if !ok || !singleTestingParam(fd.Type.Params, testingNames, kind) {
			continue
		}
		tests = append(tests, Test{
			ID:       "go:" + importPath + ":" + fd.Name.Name,
			Language: "go",
			Package:  importPath,
			Name:     fd.Name.Name,
			File:     rel,
			RunnerID: fd.Name.Name,
			Line:     fset.Position(fd.Pos()).Line,
			Purpose:  goPurpose(fd),
			Asserts:  goAsserts(fset, fd, testingNames),
		})
	}
	return tests, nil
}

// testKind returns "T" for Test functions and "F" for Fuzz functions whose
// suffix does not start with a lowercase letter.
func testKind(name string) (string, bool) {
	for prefix, kind := range map[string]string{"Test": "T", "Fuzz": "F"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			if rest != "" {
				if first := []rune(rest)[0]; unicode.IsLower(first) {
					return "", false
				}
			}
			return kind, true
		}
	}
	return "", false
}

// testingImportNames returns the local names bound to package testing. A dot
// import is reported as ".".
func testingImportNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, spec := range file.Imports {
		if spec.Path.Value != `"testing"` {
			continue
		}
		switch {
		case spec.Name == nil:
			names["testing"] = true
		case spec.Name.Name != "_":
			names[spec.Name.Name] = true
		}
	}
	return names
}

// isTestingType reports whether expr is *testing.<kind> for one of kinds.
func isTestingType(expr ast.Expr, names map[string]bool, kinds ...string) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	var typeName string
	switch x := star.X.(type) {
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok || !names[pkg.Name] {
			return false
		}
		typeName = x.Sel.Name
	case *ast.Ident:
		if !names["."] {
			return false
		}
		typeName = x.Name
	default:
		return false
	}
	for _, kind := range kinds {
		if typeName == kind {
			return true
		}
	}
	return false
}

func singleTestingParam(params *ast.FieldList, names map[string]bool, kind string) bool {
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) > 1 {
		return false
	}
	return isTestingType(params.List[0].Type, names, kind)
}

// goPurpose is the first sentence of the doc comment, else the name split on
// CamelCase boundaries and underscores.
func goPurpose(fd *ast.FuncDecl) string {
	if fd.Doc != nil {
		text := strings.Join(strings.Fields(fd.Doc.Text()), " ")
		if text != "" {
			if i := strings.Index(text, ". "); i >= 0 {
				return text[:i+1]
			}
			return text
		}
	}
	return splitCamel(fd.Name.Name)
}

func splitCamel(name string) string {
	runes := []rune(name)
	var words []string
	var word []rune
	flush := func() {
		if len(word) > 0 {
			words = append(words, string(word))
			word = word[:0]
		}
	}
	for i, r := range runes {
		if r == '_' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		word = append(word, r)
	}
	flush()
	return strings.Join(words, " ")
}

// goAsserts returns up to maxAsserts conditions of if statements whose body
// directly calls Error* or Fatal* on a testing value, in source order.
func goAsserts(fset *token.FileSet, fd *ast.FuncDecl, testingNames map[string]bool) []string {
	receivers := map[string]bool{}
	ast.Inspect(fd, func(n ast.Node) bool {
		var params *ast.FieldList
		switch x := n.(type) {
		case *ast.FuncDecl:
			params = x.Type.Params
		case *ast.FuncLit:
			params = x.Type.Params
		}
		if params != nil {
			for _, field := range params.List {
				if isTestingType(field.Type, testingNames, "T", "F", "TB") {
					for _, name := range field.Names {
						receivers[name.Name] = true
					}
				}
			}
		}
		return true
	})
	var asserts []string
	seen := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || len(asserts) >= maxAsserts || !reportsFailure(stmt.Body, receivers) {
			return true
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, stmt.Cond); err != nil {
			return true
		}
		text := strings.Join(strings.Fields(buf.String()), " ")
		if runes := []rune(text); len(runes) > maxAssertRunes {
			text = string(runes[:maxAssertRunes])
		}
		if text != "" && !seen[text] {
			seen[text] = true
			asserts = append(asserts, text)
		}
		return true
	})
	return asserts
}

// reportsFailure reports whether a statement directly in body calls
// recv.Error*/recv.Fatal*.
func reportsFailure(body *ast.BlockStmt, receivers map[string]bool) bool {
	for _, stmt := range body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		recv, ok := sel.X.(*ast.Ident)
		if ok && receivers[recv.Name] && (strings.HasPrefix(sel.Sel.Name, "Error") || strings.HasPrefix(sel.Sel.Name, "Fatal")) {
			return true
		}
	}
	return false
}
