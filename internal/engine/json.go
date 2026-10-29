package engine

import (
	"encoding/json"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/impact"
)

// The Decision wire format uses snake_case keys. The change and impact
// packages carry no JSON tags, so the engine owns the mapping. Hunk text is
// never emitted.

type selectedJSON struct {
	TestID   string   `json:"test_id"`
	Source   string   `json:"source"`
	Reason   string   `json:"reason"`
	P        *float64 `json:"p,omitempty"`
	Package  string   `json:"package,omitempty"`
	RunnerID string   `json:"runner_id,omitempty"`
}

type hunkJSON struct {
	OldStart int      `json:"old_start"`
	OldLines int      `json:"old_lines"`
	NewStart int      `json:"new_start"`
	NewLines int      `json:"new_lines"`
	Symbols  []string `json:"symbols"`
}

type fileJSON struct {
	Path     string     `json:"path"`
	OldPath  string     `json:"old_path,omitempty"`
	Kind     string     `json:"kind"`
	Status   string     `json:"status"`
	Language string     `json:"language"`
	Binary   bool       `json:"binary"`
	Package  string     `json:"package"`
	Hunks    []hunkJSON `json:"hunks"`
}

type changeJSON struct {
	BaseSHA      string     `json:"base_sha"`
	HeadSHA      string     `json:"head_sha"`
	Files        []fileJSON `json:"files"`
	LinesAdded   int        `json:"lines_added"`
	LinesRemoved int        `json:"lines_removed"`
	TriggersFull []string   `json:"triggers_full"`
	Dirty        bool       `json:"dirty"`
}

type memberJSON struct {
	TestID string `json:"test_id"`
	Reason string `json:"reason"`
}

type featuresJSON struct {
	Hops              int     `json:"hops"`
	Reachable         bool    `json:"reachable"`
	CoverageOverlap   float64 `json:"coverage_overlap"`
	CoverageKnown     bool    `json:"coverage_known"`
	CoverageStale     bool    `json:"coverage_stale"`
	CofailureRate     float64 `json:"cofailure_rate"`
	CofailureSupport  int     `json:"cofailure_support"`
	TestFailRate      float64 `json:"test_fail_rate"`
	DaysSinceLastFail float64 `json:"days_since_last_fail"`
	SharedSymbolHits  int     `json:"shared_symbol_hits"`
	SameFile          bool    `json:"same_file"`
	SamePackage       bool    `json:"same_package"`
	ChangedFileKinds  []int   `json:"changed_file_kinds"`
	LinesAdded        int     `json:"lines_added"`
	LinesRemoved      int     `json:"lines_removed"`
	FilesChanged      int     `json:"files_changed"`
	TestMedianMS      int     `json:"test_median_ms"`
	TestIsIntegration bool    `json:"test_is_integration"`
}

type candidateJSON struct {
	TestID   string       `json:"test_id"`
	TestName string       `json:"test_name"`
	TestFile string       `json:"test_file"`
	Package  string       `json:"package"`
	Purpose  string       `json:"purpose"`
	Asserts  []string     `json:"asserts"`
	Features featuresJSON `json:"features"`
	Rank     float64      `json:"rank"`
}

type l1JSON struct {
	Certain         []memberJSON    `json:"certain"`
	Candidates      []candidateJSON `json:"candidates"`
	RawCandidates   int             `json:"raw_candidates"`
	Truncated       int             `json:"truncated"`
	ChangedPackages []string        `json:"changed_packages"`
	PackageCount    int             `json:"package_count"`
	TestHops        map[string]int  `json:"test_hops"`
	CoverageState   string          `json:"coverage_state"`
	FullReason      string          `json:"full_reason"`
}

type decisionJSON struct {
	ID            string         `json:"id"`
	FullSuite     bool           `json:"full_suite"`
	Abstained     bool           `json:"abstained"`
	AbstainReason string         `json:"abstain_reason"`
	LockDigest    string         `json:"lock_digest"`
	Lambda        float64        `json:"lambda"`
	Tests         []selectedJSON `json:"tests"`
	Hypothetical  []selectedJSON `json:"hypothetical"`
	AllTests      []string       `json:"all_tests"`
	Change        *changeJSON    `json:"change"`
	L1            *l1JSON        `json:"l1"`
	Warnings      []string       `json:"warnings"`
	EstMSSelected int            `json:"est_ms_selected"`
	EstMSFull     int            `json:"est_ms_full"`
	OverheadMS    int            `json:"overhead_ms"`
	CostUSD       float64        `json:"cost_usd"`
	Payload       string         `json:"payload,omitempty"`
}

// MarshalJSON encodes the Decision with snake_case keys. Tests, AllTests and
// Warnings are always arrays. Hypothetical is null when L1 itself demanded the
// full suite and an array (possibly empty) when L1 produced a selection.
func (d Decision) MarshalJSON() ([]byte, error) {
	out := decisionJSON{
		ID:            d.ID,
		FullSuite:     d.FullSuite,
		Abstained:     d.Abstained,
		AbstainReason: d.AbstainReason,
		LockDigest:    d.LockDigest,
		Lambda:        d.Lambda,
		Tests:         selectedList(d.Tests, false),
		Hypothetical:  selectedList(d.Hypothetical, d.Hypothetical == nil),
		AllTests:      orEmpty(d.AllTests),
		Change:        changeToJSON(d.Change),
		L1:            l1ToJSON(d.L1),
		Warnings:      orEmpty(d.Warnings),
		EstMSSelected: d.EstMSSelected,
		EstMSFull:     d.EstMSFull,
		OverheadMS:    d.OverheadMS,
		CostUSD:       d.CostUSD,
		Payload:       string(d.Payload),
	}
	return json.Marshal(out)
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// selectedList converts a selection; asNull keeps a nil list null.
func selectedList(list []Selected, asNull bool) []selectedJSON {
	if asNull {
		return nil
	}
	out := make([]selectedJSON, len(list))
	for i, s := range list {
		out[i] = selectedJSON(s) // identical fields, so the wire format cannot drift from Selected
	}
	return out
}

func changeToJSON(cs *change.ChangeSet) *changeJSON {
	if cs == nil {
		return nil
	}
	out := &changeJSON{
		BaseSHA:      cs.BaseSHA,
		HeadSHA:      cs.HeadSHA,
		Files:        make([]fileJSON, len(cs.Files)),
		LinesAdded:   cs.LinesAdded,
		LinesRemoved: cs.LinesRemoved,
		TriggersFull: orEmpty(cs.TriggersFull),
		Dirty:        cs.Dirty,
	}
	for i, f := range cs.Files {
		hunks := make([]hunkJSON, len(f.Hunks))
		for j, h := range f.Hunks {
			hunks[j] = hunkJSON{OldStart: h.OldStart, OldLines: h.OldLines, NewStart: h.NewStart, NewLines: h.NewLines, Symbols: orEmpty(h.Symbols)}
		}
		out.Files[i] = fileJSON{Path: f.Path, OldPath: f.OldPath, Kind: f.Kind.String(), Status: f.Status, Language: f.Language, Binary: f.Binary, Package: f.Package, Hunks: hunks}
	}
	return out
}

func l1ToJSON(r *impact.Result) *l1JSON {
	if r == nil {
		return nil
	}
	out := &l1JSON{
		Certain:         make([]memberJSON, len(r.Certain)),
		Candidates:      make([]candidateJSON, len(r.Candidates)),
		RawCandidates:   r.RawCandidates,
		Truncated:       r.Truncated,
		ChangedPackages: orEmpty(r.ChangedPackages),
		PackageCount:    r.PackageCount,
		TestHops:        r.TestHops,
		CoverageState:   r.CoverageState,
		FullReason:      r.FullReason,
	}
	if out.TestHops == nil {
		out.TestHops = map[string]int{}
	}
	for i, m := range r.Certain {
		out.Certain[i] = memberJSON{TestID: m.TestID, Reason: m.Reason}
	}
	for i, c := range r.Candidates {
		f := c.Features
		out.Candidates[i] = candidateJSON{
			TestID: c.TestID, TestName: c.TestName, TestFile: c.TestFile, Package: c.Package, Purpose: c.Purpose,
			Asserts: orEmpty(c.Asserts), Rank: c.Rank,
			Features: featuresJSON{
				Hops: f.Hops, Reachable: f.Reachable, CoverageOverlap: f.CoverageOverlap, CoverageKnown: f.CoverageKnown,
				CoverageStale: f.CoverageStale, CofailureRate: f.CofailureRate, CofailureSupport: f.CofailureSupport,
				TestFailRate: f.TestFailRate, DaysSinceLastFail: f.DaysSinceLastFail, SharedSymbolHits: f.SharedSymbolHits,
				SameFile: f.SameFile, SamePackage: f.SamePackage, ChangedFileKinds: f.ChangedFileKinds[:],
				LinesAdded: f.LinesAdded, LinesRemoved: f.LinesRemoved, FilesChanged: f.FilesChanged,
				TestMedianMS: f.TestMedianMS, TestIsIntegration: f.TestIsIntegration,
			},
		}
	}
	return out
}
