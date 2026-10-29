package change

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Interval is a named declaration spanning 1-based inclusive lines.
type Interval struct {
	Name       string
	Start, End int
}

// GoSymbols returns the top-level declarations of src in source order:
// functions ("F"), methods ("T.M" or "(*T).M", type parameters dropped), types,
// vars and consts. A declaration interval starts at its doc comment. Specs of a
// parenthesised var or type group are separate intervals. A const group that
// relies on implicit repetition (iota) is one interval shared by every name in
// it, because editing one line can change the value of the others. Line
// directives are ignored. A syntax error is returned and no intervals.
func GoSymbols(src []byte) ([]Interval, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	line := func(p token.Pos) int { return fset.PositionFor(p, false).Line }
	var out []Interval
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			start := d.Pos()
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
			out = append(out, Interval{funcName(d), line(start), line(d.End())})
		case *ast.GenDecl:
			out = appendGenDecl(out, d, line)
		}
	}
	return out, nil
}

func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	pointer := false
	recv := d.Recv.List[0].Type
	for {
		switch x := recv.(type) {
		case *ast.ParenExpr:
			recv = x.X
			continue
		case *ast.StarExpr:
			pointer = true
			recv = x.X
			continue
		case *ast.IndexExpr:
			recv = x.X
			continue
		case *ast.IndexListExpr:
			recv = x.X
			continue
		}
		break
	}
	name := "?"
	if id, ok := recv.(*ast.Ident); ok {
		name = id.Name
	}
	if pointer {
		return "(*" + name + ")." + d.Name.Name
	}
	return name + "." + d.Name.Name
}

func appendGenDecl(out []Interval, d *ast.GenDecl, line func(token.Pos) int) []Interval {
	if d.Tok == token.IMPORT {
		return out
	}
	declStart := d.Pos()
	if d.Doc != nil {
		declStart = d.Doc.Pos()
	}
	grouped := d.Lparen.IsValid()
	implicit := false
	if d.Tok == token.CONST && grouped {
		for _, s := range d.Specs {
			if v, ok := s.(*ast.ValueSpec); ok && len(v.Values) == 0 {
				implicit = true
			}
		}
	}
	for _, s := range d.Specs {
		var names []*ast.Ident
		var doc *ast.CommentGroup
		switch s := s.(type) {
		case *ast.ValueSpec:
			names, doc = s.Names, s.Doc
		case *ast.TypeSpec:
			names, doc = []*ast.Ident{s.Name}, s.Doc
		}
		start, end := s.Pos(), d.End()
		switch {
		case implicit:
			start = declStart
		case !grouped:
			start = declStart
			end = s.End()
		default:
			if doc != nil {
				start = doc.Pos()
			}
			end = s.End()
		}
		for _, n := range names {
			out = append(out, Interval{n.Name, line(start), line(end)})
		}
	}
	return out
}

// PythonSymbols returns the top-level def, async def and class statements of
// src in source order. Decorators belong to the declaration they precede. The
// end of a declaration is the last line of its body, found by indentation with
// strings, comments, brackets and backslash continuations understood so that
// a continuation line at column 0 does not end the body. Comment and blank
// lines after the last statement of a body belong to no declaration.
func PythonSymbols(src []byte) []Interval {
	lines := strings.Split(string(src), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var (
		out     []Interval
		sc      pyScanner
		pending int // first decorator line waiting for its declaration
		open    = -1
		last    int // last content line of the open declaration
	)
	closeDecl := func() {
		if open >= 0 {
			out[open].End = last
			open = -1
		}
	}
	for i, text := range lines {
		text = strings.TrimSuffix(text, "\r")
		n := i + 1
		if sc.atLogicalStart() {
			trimmed := strings.TrimLeft(text, " \t\f")
			if trimmed == "" || trimmed[0] == '#' {
				continue
			}
			if len(trimmed) == len(text) {
				if name, ok := pyDeclName(trimmed); ok {
					closeDecl()
					start := n
					if pending != 0 {
						start = pending
					}
					pending = 0
					out = append(out, Interval{name, start, n})
					open, last = len(out)-1, n
				} else if trimmed[0] == '@' {
					closeDecl()
					if pending == 0 {
						pending = n
					}
				} else {
					closeDecl()
					pending = 0
				}
			} else if open >= 0 {
				last = n
			}
		} else if open >= 0 {
			last = n
		}
		sc.scan(text)
	}
	closeDecl()
	return out
}

// pyDeclName returns the name of a def, async def or class statement.
func pyDeclName(line string) (string, bool) {
	rest, ok := cutKeyword(line, "async")
	if ok {
		if rest, ok = cutKeyword(rest, "def"); !ok {
			return "", false
		}
	} else if rest, ok = cutKeyword(line, "def"); !ok {
		if rest, ok = cutKeyword(line, "class"); !ok {
			return "", false
		}
	}
	end := 0
	for end < len(rest) {
		r, size := utf8.DecodeRuneInString(rest[end:])
		if r != '_' && !unicode.IsLetter(r) && (end == 0 || !unicode.IsDigit(r)) {
			break
		}
		end += size
	}
	if end == 0 {
		return "", false
	}
	return rest[:end], true
}

// cutKeyword reports whether s starts with kw followed by a space or tab and
// returns the text after the whitespace that follows it.
func cutKeyword(s, kw string) (string, bool) {
	if len(s) > len(kw) && strings.HasPrefix(s, kw) && (s[len(kw)] == ' ' || s[len(kw)] == '\t') {
		return strings.TrimLeft(s[len(kw):], " \t"), true
	}
	return "", false
}

// pyScanner tracks the lexical state that decides whether a physical line
// starts a new logical line: bracket depth, an open triple-quoted string, a
// single-quoted string continued by a backslash, and a backslash continuation.
type pyScanner struct {
	depth  int
	triple string
	quote  byte
	cont   bool
}

func (s *pyScanner) atLogicalStart() bool {
	return s.depth == 0 && s.triple == "" && s.quote == 0 && !s.cont
}

func (s *pyScanner) scan(line string) {
	s.cont = false
	for i := 0; i < len(line); {
		c := line[i]
		if s.triple != "" {
			switch {
			case c == '\\':
				i += 2
			case strings.HasPrefix(line[i:], s.triple):
				s.triple = ""
				i += 3
			default:
				i++
			}
			continue
		}
		if s.quote != 0 {
			switch {
			case c == '\\' && i+1 >= len(line):
				return // backslash-newline keeps the string open
			case c == '\\':
				i += 2
			case c == s.quote:
				s.quote = 0
				i++
			default:
				i++
			}
			continue
		}
		switch c {
		case '#':
			return
		case '\'', '"':
			if strings.HasPrefix(line[i:], strings.Repeat(string(c), 3)) {
				s.triple = line[i : i+3]
				i += 3
			} else {
				s.quote = c
				i++
			}
		case '(', '[', '{':
			s.depth++
			i++
		case ')', ']', '}':
			if s.depth > 0 {
				s.depth--
			}
			i++
		case '\\':
			s.cont = i == len(line)-1
			i++
		default:
			i++
		}
	}
	s.quote = 0 // an unterminated single-line string ends with its line
}
