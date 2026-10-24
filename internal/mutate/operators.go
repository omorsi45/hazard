package mutate

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"math/big"
	"slices"
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
	// skipLit holds literals no const mutant may touch: import paths, struct
	// tags, array lengths, string constants and constants used as array lengths.
	skipLit map[*ast.BasicLit]bool
	// lastStmt holds the final statement of every function that has results.
	lastStmt map[ast.Stmt]bool
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
	g := &goGen{
		b:        newBuilder(file, src),
		tf:       fset.File(f.Pos()),
		enabled:  enabled,
		skipLit:  map[*ast.BasicLit]bool{},
		lastStmt: map[ast.Stmt]bool{},
	}
	g.prepare(f)
	ast.Inspect(f, g.visit)
	if g.b.err != nil {
		return nil, g.b.err
	}
	var mutants []Mutant
	for _, m := range g.b.sorted() {
		if stillParses(src, m) {
			mutants = append(mutants, m)
		}
	}
	return mutants, nil
}

// stillParses reports whether the file is syntactically valid with m applied.
// It does not type-check: the driver discards mutants that fail to build.
func stillParses(src []byte, m Mutant) bool {
	out, err := Apply(src, m)
	if err != nil {
		return false
	}
	_, err = parser.ParseFile(token.NewFileSet(), m.File, out, parser.SkipObjectResolution)
	return err == nil
}

// prepare indexes the context the operators need before the main walk.
func (g *goGen) prepare(f *ast.File) {
	arrayConsts := map[string]bool{} // identifiers appearing in an array length
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ImportSpec:
			g.skipLit[n.Path] = true
		case *ast.Field:
			if n.Tag != nil {
				g.skipLit[n.Tag] = true
			}
		case *ast.ArrayType:
			if n.Len != nil {
				ast.Inspect(n.Len, func(m ast.Node) bool {
					switch m := m.(type) {
					case *ast.Ident:
						arrayConsts[m.Name] = true
					case *ast.BasicLit:
						g.skipLit[m] = true
					}
					return true
				})
			}
		case *ast.FuncDecl:
			g.protectLast(n.Type, n.Body)
		case *ast.FuncLit:
			g.protectLast(n.Type, n.Body)
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			usedAsLength := slices.ContainsFunc(vs.Names, func(id *ast.Ident) bool { return arrayConsts[id.Name] })
			for _, v := range vs.Values {
				ast.Inspect(v, func(m ast.Node) bool {
					if lit, ok := m.(*ast.BasicLit); ok && (lit.Kind == token.STRING || usedAsLength) {
						g.skipLit[lit] = true
					}
					return true
				})
			}
		}
		return true
	})
}

// protectLast remembers the last statement of a function that has results:
// deleting it would leave the function without a terminating statement.
func (g *goGen) protectLast(typ *ast.FuncType, body *ast.BlockStmt) {
	if body == nil || len(body.List) == 0 || typ.Results.NumFields() == 0 {
		return
	}
	g.lastStmt[body.List[len(body.List)-1]] = true
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
	case *ast.BasicLit:
		g.literal(n)
	case *ast.FuncDecl:
		g.returns(n.Type, n.Body)
	case *ast.FuncLit:
		g.returns(n.Type, n.Body)
	case *ast.BlockStmt:
		g.deleteStatements(n.List)
	case *ast.CaseClause:
		g.deleteStatements(n.Body)
	case *ast.CommClause:
		g.deleteStatements(n.Body)
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
	u, ok := unparen(e).(*ast.UnaryExpr)
	return ok && u.Op == token.NOT
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// literal records the const mutants of one literal: a decimal integer n
// becomes n+1, n-1 and (when n is not 0) 0; a non-empty string becomes "".
func (g *goGen) literal(lit *ast.BasicLit) {
	if !g.enabled["const"] || g.skipLit[lit] {
		return
	}
	start := g.tf.Offset(lit.Pos())
	switch lit.Kind {
	case token.INT:
		n, ok := decimal(lit.Value)
		if !ok {
			return
		}
		for _, d := range []int64{1, -1} {
			g.b.add("const", start, len(lit.Value), new(big.Int).Add(n, big.NewInt(d)).String())
		}
		if n.Sign() != 0 {
			g.b.add("const", start, len(lit.Value), "0")
		}
	case token.STRING:
		if len(lit.Value) <= 2 { // "" or an empty raw string
			return
		}
		g.b.add("const", start, g.stringLength(start, lit), `""`)
	}
}

// stringLength is the number of source bytes of a string literal. The parser
// drops carriage returns from raw string values, so a raw string is measured
// in the source itself.
func (g *goGen) stringLength(start int, lit *ast.BasicLit) int {
	if lit.Value[0] != '`' {
		return len(lit.Value)
	}
	if i := bytes.IndexByte(g.b.src[start+1:], '`'); i >= 0 {
		return i + 2
	}
	return len(lit.Value)
}

// decimal parses a plain decimal integer literal: digits only, no base prefix,
// no digit separators, no legacy octal.
func decimal(s string) (*big.Int, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return nil, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return nil, false
		}
	}
	return new(big.Int).SetString(s, 10)
}

// returns records the ret mutants of one function: every result expression of
// its return statements becomes the zero value of the declared result type.
// Functions with any result type outside the supported set are skipped whole.
func (g *goGen) returns(typ *ast.FuncType, body *ast.BlockStmt) {
	if !g.enabled["ret"] || body == nil {
		return
	}
	zeros, ok := resultZeros(typ)
	if !ok {
		return
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false // its returns belong to the literal, visited on its own
		case *ast.ReturnStmt:
			g.zeroResults(n, zeros)
		}
		return true
	})
}

func (g *goGen) zeroResults(ret *ast.ReturnStmt, zeros []string) {
	if len(ret.Results) != len(zeros) { // bare return, or one call yielding a tuple
		return
	}
	for i, e := range ret.Results {
		if isZero(e, zeros[i]) {
			continue
		}
		start, end := g.tf.Offset(e.Pos()), g.tf.Offset(e.End())
		g.b.add("ret", start, end-start, zeros[i])
	}
}

// resultZeros returns the zero value of each result of typ, in order, or false
// if there are no results or any result has an unsupported type.
func resultZeros(typ *ast.FuncType) ([]string, bool) {
	if typ.Results == nil {
		return nil, false
	}
	var zeros []string
	for _, field := range typ.Results.List {
		z, ok := zeroValue(field.Type)
		if !ok {
			return nil, false
		}
		for range max(1, len(field.Names)) {
			zeros = append(zeros, z)
		}
	}
	return zeros, len(zeros) > 0
}

// zeroValue is the zero value of a result type, for bool, string, the numeric
// builtin types, error and the nilable type constructors.
func zeroValue(t ast.Expr) (string, bool) {
	switch t := t.(type) {
	case *ast.ParenExpr:
		return zeroValue(t.X)
	case *ast.Ident:
		switch t.Name {
		case "bool":
			return "false", true
		case "string":
			return `""`, true
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
			"float32", "float64", "complex64", "complex128", "byte", "rune":
			return "0", true
		case "error", "any":
			return "nil", true
		}
	case *ast.StarExpr, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return "nil", true
	case *ast.ArrayType:
		if t.Len == nil { // a slice; an array's zero value is not nil
			return "nil", true
		}
	}
	return "", false
}

// isZero reports whether e already is the zero value written as zero.
func isZero(e ast.Expr, zero string) bool {
	switch e := unparen(e).(type) {
	case *ast.Ident:
		return (zero == "nil" || zero == "false") && e.Name == zero
	case *ast.BasicLit:
		v := constant.MakeFromLiteral(e.Value, e.Kind, 0)
		switch v.Kind() {
		case constant.String:
			return zero == `""` && constant.StringVal(v) == ""
		case constant.Int, constant.Float, constant.Complex:
			return zero == "0" && constant.Sign(v) == 0
		}
	}
	return false
}

// deleteStatements records the stmt mutants of one statement list.
func (g *goGen) deleteStatements(list []ast.Stmt) {
	if !g.enabled["stmt"] {
		return
	}
	for _, s := range list {
		if g.lastStmt[s] || !deletable(s) {
			continue
		}
		start, end := g.tf.Offset(s.Pos()), g.tf.Offset(s.End())
		g.b.add("stmt", start, end-start, "")
	}
}

// deletable reports whether s is a call, an increment or decrement, or a plain
// "=" assignment. Declarations, ":=", return, defer, go and labeled
// statements are never deleted.
func deletable(s ast.Stmt) bool {
	switch s := s.(type) {
	case *ast.ExprStmt:
		_, isCall := s.X.(*ast.CallExpr)
		return isCall
	case *ast.IncDecStmt:
		return true
	case *ast.AssignStmt:
		return s.Tok == token.ASSIGN
	}
	return false
}
