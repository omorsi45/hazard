package redact

import (
	"fmt"
	"strings"

	"github.com/omorsi45/hazard/internal/change"
)

// maxShownSymbols is the number of declared symbols a summary names before it
// counts the rest.
const maxShownSymbols = 5

// Phrases of the summary. They are the only text a summary takes besides
// counts and the declared symbol names, which is what keeps source text out.
var (
	classPhrases = [numClasses]string{
		classCompare: "comparison operator changed",
		classLogical: "logical operator changed",
		classArith:   "arithmetic operator changed",
		classLiteral: "literal changed",
		classIdent:   "identifier changed",
		classKeyword: "control flow changed",
	}
	phraseComment     = "comment changed"
	phraseAdded       = "lines added"
	phraseRemoved     = "lines removed"
	phraseWhitespace  = "whitespace changed"
	phraseReordered   = "code reordered"
	phrasePunctuation = "punctuation changed"

	// phraseOrder fixes the order of the categories in a summary.
	phraseOrder = []string{
		classPhrases[classCompare], classPhrases[classLogical], classPhrases[classArith],
		classPhrases[classLiteral], classPhrases[classIdent], classPhrases[classKeyword],
		phraseComment, phraseAdded, phraseRemoved, phraseWhitespace, phraseReordered, phrasePunctuation,
	}
)

// summarize returns the one-line summary of a changed file. declared holds the
// names to show for the declarations the hunks touch, already hashed when the
// privacy level asks for it.
func summarize(f change.ChangedFile, declared []string) string {
	if f.Binary {
		return "binary file " + statusWord(f.Status)
	}
	switch f.Status {
	case "A":
		return fmt.Sprintf("file added, %s", count(sumLines(f.Hunks, func(h change.Hunk) int { return h.NewLines }), "line"))
	case "D":
		return fmt.Sprintf("file deleted, %s", count(sumLines(f.Hunks, func(h change.Hunk) int { return h.OldLines }), "line"))
	}
	prefix := ""
	if f.Status == "R" {
		prefix = "file renamed"
		if len(f.Hunks) == 0 {
			return prefix
		}
		prefix += "; "
	} else if len(f.Hunks) == 0 {
		return "file " + statusWord(f.Status) + " without line changes"
	}
	return prefix + editSummary(f, declared)
}

func statusWord(status string) string {
	switch status {
	case "A":
		return "added"
	case "D":
		return "deleted"
	case "R":
		return "renamed"
	case "M":
		return "modified"
	default:
		return "changed"
	}
}

// editSummary describes the edited lines of a file: how many, where and what
// kind of token changed.
func editSummary(f change.ChangedFile, declared []string) string {
	n := sumLines(f.Hunks, func(h change.Hunk) int { return max(h.OldLines, h.NewLines) })
	where := "outside any declaration"
	if len(declared) > 0 {
		where = "in " + nameList(declared)
	}
	out := fmt.Sprintf("%s changed %s", count(n, "line"), where)
	if cats := categories(f); len(cats) > 0 {
		out += ": " + strings.Join(cats, ", ")
	}
	return out
}

func nameList(names []string) string {
	if len(names) <= maxShownSymbols {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxShownSymbols], ", "), len(names)-maxShownSymbols)
}

func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func sumLines(hunks []change.Hunk, lines func(change.Hunk) int) int {
	total := 0
	for _, h := range hunks {
		total += lines(h)
	}
	return total
}

// categories returns the phrases that describe the hunks of f, without
// duplicates and in the fixed phraseOrder.
func categories(f change.ChangedFile) []string {
	found := map[string]bool{}
	for _, h := range f.Hunks {
		for _, phrase := range hunkCategories(h.Text, f.Language) {
			found[phrase] = true
		}
	}
	var out []string
	for _, phrase := range phraseOrder {
		if found[phrase] {
			out = append(out, phrase)
		}
	}
	return out
}

// hunkCategories compares the removed and the added lines of one hunk token
// class by token class. A class is reported when a token of it was replaced,
// or, for every class except identifiers, when one was added or dropped:
// new identifiers come with any new code, new operators, literals and control
// flow are behavior. Only fixed phrases leave this function, never a token.
func hunkCategories(text, lang string) []string {
	var removed, added []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		switch line[0] {
		case '-':
			removed = append(removed, line[1:])
		case '+':
			added = append(added, line[1:])
		}
	}
	if len(removed) == 0 && len(added) == 0 {
		return nil
	}
	old, now := lex(removed, lang), lex(added, lang)
	switch {
	case old.blank() && now.blank():
		return []string{phraseWhitespace}
	case old.blank():
		return []string{phraseAdded}
	case now.blank():
		return []string{phraseRemoved}
	}

	var out []string
	for c := class(0); c < numClasses; c++ {
		if classPhrases[c] == "" {
			continue
		}
		oldOnly, nowOnly := difference(old.bags[c], now.bags[c])
		if (oldOnly > 0 && nowOnly > 0) || (c != classIdent && (oldOnly > 0 || nowOnly > 0)) {
			out = append(out, classPhrases[c])
		}
	}
	if len(out) > 0 {
		return out
	}
	if a, b := difference(old.comments, now.comments); a > 0 || b > 0 {
		return []string{phraseComment}
	}
	if equalSequences(old.sequence, now.sequence) {
		return []string{phraseWhitespace}
	}
	if a, b := difference(old.all, now.all); a == 0 && b == 0 {
		return []string{phraseReordered}
	}
	return []string{phrasePunctuation}
}

// difference counts the elements by which each bag exceeds the other.
func difference(a, b map[string]int) (aOnly, bOnly int) {
	for k, n := range a {
		if n > b[k] {
			aOnly += n - b[k]
		}
	}
	for k, n := range b {
		if n > a[k] {
			bOnly += n - a[k]
		}
	}
	return aOnly, bOnly
}

func equalSequences(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
