package mutate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/graph"
)

// Options configures Run.
type Options struct {
	RepoDir     string        // git repository; only Out and DBPath are ever written in it
	Base        string        // revision whose tracked files are mutated
	Files       []string      // globs (change.MatchGlob) selecting candidate files
	N           int           // execute the first N candidates by ID; <= 0 means all
	Operators   []string      // from ParseOperators; empty is an error
	Timeout     time.Duration // per-mutant `go test -timeout`
	Concurrency int           // number of slots; values < 1 mean 1
	Packages    []string      // go package patterns, e.g. ./...
	Tags        []string      // go build tags
	MaxHops     int           // reverse-import distance beyond which a killer is "beyond max hops"
	Out         string        // JSONL path; empty means no file
	DBPath      string        // SQLite path; empty means no database
	Log         io.Writer     // progress lines; nil discards
}

// Record is the outcome of one executed mutant.
type Record struct {
	ID       string `json:"id"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Operator string `json:"operator"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
	BaseSHA  string `json:"base_sha"`
	// Status is killed, survived, build_error, timeout or unconfirmed.
	Status string `json:"status"`
	// Killers are the tests that failed in both runs (empty unless killed).
	Killers []string `json:"killers"`
	// UnreachableKillers is the subset of Killers whose package cannot import
	// the mutated file's package, directly or transitively.
	UnreachableKillers []string `json:"unreachable_killers"`
	// BeyondMaxHopsKillers is the subset of Killers that is reachable only
	// through more than Options.MaxHops reverse import edges.
	BeyondMaxHopsKillers []string `json:"beyond_max_hops_killers"`
	DurationMS           int64    `json:"duration_ms"`
}

// Summary describes one Run.
type Summary struct {
	BaseSHA    string         `json:"base_sha"`
	Candidates int            `json:"candidates"` // mutants generated for the selected files
	Executed   int            `json:"executed"`   // mutants actually run
	ByStatus   map[string]int `json:"by_status"`
	ByOperator map[string]int `json:"by_operator"` // executed mutants per operator
	Usable     int            `json:"usable"`      // killed mutants
	// UnreachableMutants counts killed mutants with at least one killer outside
	// the unbounded reverse reachability of the mutated package.
	UnreachableMutants int `json:"unreachable_mutants"`
	// FullyUnreachableMutants counts killed mutants whose every killer is unreachable.
	FullyUnreachableMutants int `json:"fully_unreachable_mutants"`
	// BeyondMaxHopsMutants counts killed mutants with a killer reachable only
	// beyond Options.MaxHops.
	BeyondMaxHopsMutants int      `json:"beyond_max_hops_mutants"`
	BaselineSeconds      float64  `json:"baseline_seconds"`
	Records              []Record `json:"records,omitempty"`
}

// Run executes the mutation driver.
func Run(ctx context.Context, opts Options) (Summary, error) {
	return Summary{}, errors.New("mutate: not implemented")
}

// goToolIgnores reports whether the go tool ignores the directory holding rel:
// testdata, vendor, and directories starting with "." or "_".
func goToolIgnores(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		if seg == "testdata" || seg == "vendor" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_") {
			return true
		}
	}
	return false
}

// configMutantsSafe is ConfigMutants with its internal-error panic converted
// to an error, so one file cannot take the run down.
func configMutantsSafe(file string, src []byte) (ms []Mutant, err error) {
	defer func() {
		if r := recover(); r != nil {
			ms, err = nil, fmt.Errorf("mutate: config mutants of %s: %v", file, r)
		}
	}()
	return ConfigMutants(file, src), nil
}

// candidateMutants generates the mutants of the tracked files that match globs,
// ordered by ID. Go files (outside testdata, vendor and hidden directories) use
// GoMutants; with the cfg operator selected, configuration files use
// ConfigMutants. An empty operator list, an unreadable file and an unparsable
// Go file are errors.
func candidateMutants(tracked, globs, operators []string, read func(rel string) ([]byte, error)) ([]Mutant, error) {
	if len(operators) == 0 {
		return nil, errors.New("mutate: no operators selected (valid: " + strings.Join(allOperators, ", ") + ")")
	}
	cfgSelected := slices.Contains(operators, "cfg")
	var out []Mutant
	for _, rel := range tracked {
		if !slices.ContainsFunc(globs, func(g string) bool { return change.MatchGlob(g, rel) }) {
			continue
		}
		isGo := strings.HasSuffix(rel, ".go") && !goToolIgnores(rel)
		isCfg := cfgSelected && cfgFormatOf(rel) != cfgNone
		if !isGo && !isCfg {
			continue
		}
		src, err := read(rel)
		if err != nil {
			return nil, fmt.Errorf("mutate: read %s: %w", rel, err)
		}
		var ms []Mutant
		if isGo {
			ms, err = GoMutants(rel, src, operators)
		} else {
			ms, err = configMutantsSafe(rel, src)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	slices.SortFunc(out, func(a, b Mutant) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// limitCandidates keeps the first n mutants; n <= 0 keeps all.
func limitCandidates(ms []Mutant, n int) []Mutant {
	if n <= 0 || n >= len(ms) {
		return ms
	}
	return ms[:n]
}

// killerPackage returns the import path of a "go:<pkg>:<Test>" test ID.
func killerPackage(id string) string {
	rest := strings.TrimPrefix(id, "go:")
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// classifyKillers splits killers into those whose package cannot observe the
// mutated file's package (unbounded reverse reachability) and those reachable
// only through more than maxHops reverse import edges. A non-Go file, or a file
// the graph does not know, has no package, so every killer is unreachable.
func classifyKillers(g *graph.Graph, file string, killers []string, maxHops int) (unreachable, beyond []string) {
	var hops map[string]int
	if strings.HasSuffix(file, ".go") {
		if pkg, ok := g.PackageOfFile(file); ok {
			hops = g.ReverseHops([]string{pkg})
		}
	}
	for _, k := range killers {
		h, reachable := hops[killerPackage(k)]
		switch {
		case !reachable:
			unreachable = append(unreachable, k)
		case h > maxHops:
			beyond = append(beyond, k)
		}
	}
	return unreachable, beyond
}
