// Package impact implements L1 of selection: the deterministic CERTAIN set
// derived from a change set and the test index, the ranked residual
// candidates, and the f2 feature vector later layers consume. It is pure
// logic: it never runs git, go, a database or the network.
package impact

import (
	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/index"
)

// FeatureVersion identifies the Features layout and Vector order.
const FeatureVersion = "f2"

// Candidate is a residual test that L1 did not select with certainty.
type Candidate struct {
	TestID, TestName, TestFile, Package, Purpose string
	Asserts                                      []string
	Features                                     Features
	Rank                                         float64
}

// Member is one CERTAIN test and the first rule that selected it.
type Member struct{ TestID, Reason string }

// Reasons a test is CERTAIN, in precedence order.
const (
	reasonDirect    = "direct"
	reasonCoverage  = "coverage"
	reasonGraph     = "graph"
	reasonFixture   = "fixture"
	reasonAlwaysRun = "always_run"
)

// Mode selects the deterministic policy.
type Mode int

// Modes: ModeGraph is the graph-only baseline (B1) and ModeCoverage the
// coverage-driven policy (B2).
const (
	ModeGraph Mode = iota
	ModeCoverage
)

// History supplies per-test historical signals. Every method reports unknown
// values as zero with ok or support false; unknown is never evidence.
type History interface {
	Cofailure(testID string, changedFiles []string) (rate float64, support int)
	FailRate(testID string) float64
	DaysSinceLastFail(testID string) (days float64, ok bool)
	MedianMS(testID string) (ms int, ok bool)
}

// NoHistory is a History with no data.
type NoHistory struct{}

// Cofailure reports no co-failure evidence.
func (NoHistory) Cofailure(string, []string) (float64, int) { return 0, 0 }

// FailRate reports no failure history.
func (NoHistory) FailRate(string) float64 { return 0 }

// DaysSinceLastFail reports an unknown last failure.
func (NoHistory) DaysSinceLastFail(string) (float64, bool) { return 0, false }

// MedianMS reports an unknown runtime.
func (NoHistory) MedianMS(string) (int, bool) { return 0, false }

// Input is everything L1 reads.
type Input struct {
	Change        *change.ChangeSet
	Index         *index.Manifest
	History       History
	Impact        config.Impact
	Mode          Mode
	AlwaysRun     map[string]string // test ID -> reason, from manual quarantine and frozen evidence
	MaxCandidates int               // overrides Impact.MaxCandidates when > 0
	CoverageAge   int               // commits from Index.Coverage.HeadSHA to Change.BaseSHA; -1 when not an ancestor or unknown
	BaseDigests   map[string]string // sha256 of BASE content for every changed file that exists at base (rel path)
	ReadFile      func(rel string) ([]byte, error)
}

// Result is the L1 outcome.
type Result struct {
	Certain         []Member    // sorted by TestID, includes always_run
	Candidates      []Candidate // rank desc, ties by TestID; len <= the cap
	RawCandidates   int         // candidates before the cap
	Truncated       int         // RawCandidates minus len(Candidates)
	AllTests        []string    // full inventory IDs, sorted
	ChangedPackages []string    // graph package IDs touched (old and new paths), sorted
	PackageCount    int         // packages in the graph that own at least one test
	TestHops        map[string]int
	CoverageState   string // "fresh", "missing", "not_requested" or "stale:<reason>"
	FullReason      string // non-empty means run the full suite
}
