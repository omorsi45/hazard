package mutate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// cmpSwap maps a comparison operator to its replacement (seed 11.2, cmp).
var cmpSwap = map[token.Token]string{
	token.GTR: ">=",
	token.GEQ: ">",
	token.LSS: "<=",
	token.LEQ: "<",
	token.EQL: "!=",
	token.NEQ: "==",
}

// logicSwap maps a boolean connective to its replacement (seed 11.2, logic).
var logicSwap = map[token.Token]string{
	token.LAND: "||",
	token.LOR:  "&&",
}

// goGen walks one parsed file and records mutants in its builder.
type goGen struct {
	b       *builder
	tf      *token.File
	enabled map[string]bool
}

// GoMutants returns the mutants of one Go source file for the selected
// operators (the cfg operator belongs to ConfigMutants and is ignored here),
// sorted by Offset, then Operator, then Mutated. Test files and generated
// files yield none. A source that does not parse is an error.
func GoMutants(file string, src []byte, operators []string) ([]Mutant, error) {
	enabled := make(map[string]bool, len(operators))
	for _, op := range operators {
		if !isOperator(op) {
			return nil, fmt.Errorf("mutate: unknown operator %q (valid: %s)", op, strings.Join(allOperators, ", "))
		}
		enabled[op] = true
	}
	file = strings.ReplaceAll(file, `\`, "/")
	if strings.HasSuffix(file, "_test.go") {
		return nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("mutate: parse %s: %w", file, err)
	}
	if ast.IsGenerated(f) {
		return nil, nil
	}
	g := &goGen{b: newBuilder(file, src), tf: fset.File(f.Pos()), enabled: enabled}
	ast.Inspect(f, g.visit)
	if g.b.err != nil {
		return nil, g.b.err
	}
	return g.b.sorted(), nil
}

func (g *goGen) visit(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.BinaryExpr:
		if mut, ok := cmpSwap[n.Op]; ok {
			g.replace("cmp", n.OpPos, len(n.Op.String()), mut)
		}
		if mut, ok := logicSwap[n.Op]; ok {
			g.replace("logic", n.OpPos, len(n.Op.String()), mut)
		}
	case *ast.UnaryExpr:
		if n.Op == token.NOT {
			g.replace("logic", n.OpPos, 1, "")
		}
	case *ast.IfStmt:
		g.negateCondition(n.Cond)
	}
	return true
}

// replace records a mutant replacing length bytes at pos, if the operator is
// enabled.
func (g *goGen) replace(operator string, pos token.Pos, length int, mutated string) {
	if !g.enabled[operator] {
		return
	}
	g.b.add(operator, g.tf.Offset(pos), length, mutated)
}

// negateCondition records the logic mutant that wraps an if condition in
// "!( )", unless the condition already is a negation.
func (g *goGen) negateCondition(cond ast.Expr) {
	if !g.enabled["logic"] || isNegation(cond) {
		return
	}
	start, end := g.tf.Offset(cond.Pos()), g.tf.Offset(cond.End())
	g.b.add("logic", start, end-start, "!("+string(g.b.src[start:end])+")")
}

// isNegation reports whether e is "!x", looking through parentheses.
func isNegation(e ast.Expr) bool {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = p.X
	}
	u, ok := e.(*ast.UnaryExpr)
	return ok && u.Op == token.NOT
}
