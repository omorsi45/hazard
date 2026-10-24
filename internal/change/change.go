// Package change turns a git diff into a ChangeSet: the changed files, their
// kinds, the hunks with the declarations they touch, and the files that force
// a full test run. It uses only the standard library and the git executable.
package change

import "errors"

// FileKind classifies a changed path.
type FileKind int

// File kinds, in the order the architecture contract lists them.
const (
	KindSource FileKind = iota
	KindTest
	KindConfig
	KindFixture
	KindGenerated
	KindDocs
	KindLock
	KindBuild
)

// String returns the lower-case name of the kind.
func (k FileKind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return "unknown"
	}
	return kindNames[k]
}

var kindNames = [...]string{"source", "test", "config", "fixture", "generated", "docs", "lock", "build"}

// Hunk is one -U0 diff hunk. Symbols lists the declarations the hunk touches
// (old side from the base contents, new side from the head contents) or
// "__file__" when a range touches no declaration. Text is left empty: Options
// carries no privacy setting that would allow hunk text to leave the process.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Symbols            []string
	Text               string
}

// ChangedFile is one path in a ChangeSet.
type ChangedFile struct {
	Path, OldPath string // OldPath is set for renames
	Kind          FileKind
	Status        string // "A", "M", "D" or "R"
	Language      string // "go", "python" or ""
	Binary        bool
	Hunks         []Hunk
	Package       string // filled by the engine from the graph, empty from Extract
}

// ChangeSet is the L0 result.
type ChangeSet struct {
	BaseSHA, HeadSHA         string // HeadSHA is the HEAD commit when Dirty
	Files                    []ChangedFile
	LinesAdded, LinesRemoved int
	TriggersFull             []string // "<pattern>:<path>", sorted, old or new path
	Dirty                    bool
}

// Options selects the diff and the patterns Extract applies.
type Options struct {
	Base, Head, DefaultBranch        string
	FullSuiteTriggers, GeneratedGlobs []string
}

// ErrNoMergeBase means no trustworthy merge base exists: a shallow clone with
// the history cut, or unrelated histories.
var ErrNoMergeBase = errors.New("no merge base")
