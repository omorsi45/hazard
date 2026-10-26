package mutate

import (
	"context"
	"errors"
	"io"
	"time"
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
