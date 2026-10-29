package redact

import (
	"strconv"
	"strings"
	"unicode"
)

// class is the token class a summary compares between the removed and the
// added side of a hunk.
type class int

const (
	classCompare class = iota
	classLogical
	classArith
	classLiteral
	classIdent
	classKeyword
	classPunct
	numClasses
)

// lexed is the token view of one side of a hunk. It keeps the tokens only to
// compare the two sides; nothing in it is ever written to the summary.
type lexed struct {
	bags     [numClasses]map[string]int // token multiset per class
	all      map[string]int             // token multiset over every class
	sequence []string                   // tokens in source order
	comments map[string]int             // trimmed comment texts
}

// blank reports whether the side holds no token and no comment, which is the
// case for an empty side and for a side of empty lines.
func (l lexed) blank() bool { return len(l.sequence) == 0 && len(l.comments) == 0 }

// Language families the lexer distinguishes by their comment and string
// syntax.
const (
	langGo     = "go"
	langPython = "python"
)

var (
	controlWords = map[string]bool{
		"if": true, "else": true, "elif": true, "for": true, "while": true, "switch": true,
		"case": true, "default": true, "select": true, "return": true, "break": true,
		"continue": true, "goto": true, "fallthrough": true, "defer": true, "go": true,
		"try": true, "except": true, "finally": true, "raise": true, "match": true,
		"yield": true, "await": true,
	}
	logicalWords  = map[string]bool{"and": true, "or": true, "not": true}
	constantWords = map[string]bool{"true": true, "false": true, "nil": true, "True": true, "False": true, "None": true}

	operators3 = []string{"<<=", ">>=", "&^=", "**=", "//=", "..."}
	operators2 = []string{
		"==", "!=", "<=", ">=", "&&", "||", "++", "--", "+=", "-=", "*=", "/=", "%=",
		"&=", "|=", "^=", "<<", ">>", "&^", "**", "//", ":=", "->", "<-", "=>",
	}
)

// operatorClass classifies an operator. Pointer, address and bitwise
// operators are punctuation: they say little about the behavior of a change.
func operatorClass(op string) class {
	switch op {
	case "==", "!=", "<=", ">=", "<", ">":
		return classCompare
	case "&&", "||", "!":
		return classLogical
	case "+", "-", "*", "/", "%", "++", "--", "+=", "-=", "*=", "/=", "%=", "**", "//", "<<", ">>", "<<=", ">>=", "**=", "//=":
		return classArith
	default:
		return classPunct
	}
}

type lexToken struct {
	class class
	text  string
}

// lexer scans lines into tokens. Comment and string state carries across the
// lines of one side, so block comments, raw strings and triple-quoted strings
// that span lines are recognized.
type lexer struct {
	lang   string
	tokens []lexToken

	comments []string
	inBlock  bool
	block    strings.Builder

	rawDelim string // closing delimiter of a multi-line string, empty outside one
	raw      strings.Builder
}

// lex scans the lines of one side of a hunk.
func lex(lines []string, lang string) lexed {
	lx := &lexer{lang: lang}
	for _, line := range lines {
		lx.scan([]rune(line))
	}
	if lx.inBlock {
		lx.endBlock()
	}
	if lx.rawDelim != "" {
		lx.endRaw()
	}
	return lx.result()
}

func (lx *lexer) result() lexed {
	out := lexed{all: map[string]int{}, comments: map[string]int{}}
	for c := range out.bags {
		out.bags[c] = map[string]int{}
	}
	for _, t := range lx.tokens {
		key := strconv.Itoa(int(t.class)) + ":" + t.text
		out.bags[t.class][key]++
		out.all[key]++
		out.sequence = append(out.sequence, key)
	}
	for _, c := range lx.comments {
		out.comments[c]++
	}
	return out
}

func (lx *lexer) emit(c class, text string) { lx.tokens = append(lx.tokens, lexToken{c, text}) }

func (lx *lexer) addComment(text string) {
	lx.comments = append(lx.comments, strings.TrimSpace(text))
}

func (lx *lexer) endBlock() {
	lx.addComment(lx.block.String())
	lx.block.Reset()
	lx.inBlock = false
}

func (lx *lexer) endRaw() {
	lx.emit(classLiteral, lx.raw.String())
	lx.raw.Reset()
	lx.rawDelim = ""
}

func (lx *lexer) lineCommentAt(r []rune, i int) bool {
	switch lx.lang {
	case langGo:
		return hasAt(r, i, "//")
	case langPython:
		return r[i] == '#'
	default:
		return r[i] == '#' || hasAt(r, i, "//")
	}
}

func (lx *lexer) blockCommentAt(r []rune, i int) bool {
	return lx.lang != langPython && hasAt(r, i, "/*")
}

func (lx *lexer) scan(r []rune) {
	for i := 0; i < len(r); {
		switch {
		case lx.inBlock:
			j := indexAt(r, i, "*/")
			if j < 0 {
				lx.block.WriteString(string(r[i:]))
				lx.block.WriteByte('\n')
				return
			}
			lx.block.WriteString(string(r[i:j]))
			lx.endBlock()
			i = j + 2
		case lx.rawDelim != "":
			j := indexAt(r, i, lx.rawDelim)
			if j < 0 {
				lx.raw.WriteString(string(r[i:]))
				lx.raw.WriteByte('\n')
				return
			}
			lx.raw.WriteString(string(r[i:j]))
			i = j + len([]rune(lx.rawDelim))
			lx.endRaw()
		case unicode.IsSpace(r[i]):
			i++
		case lx.lineCommentAt(r, i):
			lx.addComment(string(r[i:]))
			return
		case lx.blockCommentAt(r, i):
			lx.inBlock = true
			i += 2
		case r[i] == '"' || r[i] == '\'' || (r[i] == '`' && lx.lang != langPython):
			i = lx.quoted(r, i)
		case unicode.IsDigit(r[i]) || (r[i] == '.' && i+1 < len(r) && unicode.IsDigit(r[i+1])):
			i = lx.number(r, i)
		case unicode.IsLetter(r[i]) || r[i] == '_':
			i = lx.word(r, i)
		default:
			i = lx.operator(r, i)
		}
	}
}

// quoted scans a string or character literal starting at r[i]. Raw strings
// and triple-quoted strings switch the lexer into multi-line state.
func (lx *lexer) quoted(r []rune, i int) int {
	q := r[i]
	switch {
	case q == '`':
		lx.rawDelim = "`"
		return i + 1
	case lx.lang == langPython && hasAt(r, i, strings.Repeat(string(q), 3)):
		lx.rawDelim = strings.Repeat(string(q), 3)
		return i + 3
	}
	j := i + 1
	for j < len(r) {
		if r[j] == '\\' {
			j += 2
			continue
		}
		j++
		if r[j-1] == q {
			break
		}
	}
	j = min(j, len(r))
	lx.emit(classLiteral, string(r[i:j]))
	return j
}

func (lx *lexer) number(r []rune, i int) int {
	j := i
	for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_' || (r[j] == '.' && !hasAt(r, j, ".."))) {
		j++
	}
	lx.emit(classLiteral, string(r[i:j]))
	return j
}

func (lx *lexer) word(r []rune, i int) int {
	j := i
	for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') {
		j++
	}
	w := string(r[i:j])
	switch {
	case controlWords[w]:
		lx.emit(classKeyword, w)
	case constantWords[w]:
		lx.emit(classLiteral, w)
	case logicalWords[w] && lx.lang != langGo:
		lx.emit(classLogical, w)
	default:
		lx.emit(classIdent, w)
	}
	return j
}

func (lx *lexer) operator(r []rune, i int) int {
	for _, op := range operators3 {
		if hasAt(r, i, op) {
			lx.emit(operatorClass(op), op)
			return i + len(op)
		}
	}
	for _, op := range operators2 {
		if hasAt(r, i, op) {
			lx.emit(operatorClass(op), op)
			return i + len(op)
		}
	}
	op := string(r[i])
	lx.emit(operatorClass(op), op)
	return i + 1
}

func hasAt(r []rune, i int, s string) bool {
	want := []rune(s)
	if i+len(want) > len(r) {
		return false
	}
	for k, w := range want {
		if r[i+k] != w {
			return false
		}
	}
	return true
}

// indexAt returns the index of the first occurrence of s in r at or after
// from, or -1.
func indexAt(r []rune, from int, s string) int {
	for i := from; i < len(r); i++ {
		if hasAt(r, i, s) {
			return i
		}
	}
	return -1
}
