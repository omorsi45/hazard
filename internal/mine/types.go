package mine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/runner"
)

// Request uses UTC half-open bounds. TokenEnv names an environment variable;
// credentials themselves belong only in the HTTP client configuration.
type Request struct {
	Provider, Project, Branch, TokenEnv, JUnitGlob, DBPath, RawDir, Language string
	Since, Until                                                             time.Time
	MaxPipelines, Concurrency                                                int
	Resume                                                                   bool
}

// Stats counts acquisition units and factual records, never regression labels.
// RawCases and FactualResults are current stored totals; ReplayDuplicates counts
// repeated raw cases. InspectedAttempts counts only known run attempt ordinals.
// InspectedArtifacts counts distinct source unit keys; incomplete/capped units
// count artifact content versions retaining those acquisition states.
type Stats struct {
	RequestedSince              time.Time      `json:"requested_since"`
	RequestedUntil              time.Time      `json:"requested_until"`
	ObservedSince               *time.Time     `json:"observed_since"`
	ObservedUntil               *time.Time     `json:"observed_until"`
	InspectedRuns               int            `json:"inspected_runs"`
	InspectedAttempts           int            `json:"inspected_attempts"`
	InspectedArtifacts          int            `json:"inspected_artifacts"`
	RawCases                    int            `json:"raw_cases"`
	FactualResults              int            `json:"factual_results"`
	FactualResultBearingCommits int            `json:"factual_result_bearing_commits"`
	ReplayDuplicates            int            `json:"replay_duplicates"`
	CappedUnits                 int            `json:"capped_units"`
	IncompleteUnits             int            `json:"incomplete_units"`
	AcquisitionExclusions       map[string]int `json:"acquisition_exclusions"`
}

// String orders exclusion reasons; encoding/json likewise sorts map keys.
func (s Stats) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "runs=%d attempts=%d artifacts=%d raw_cases=%d factual_results=%d factual_result_bearing_commits=%d replay_duplicates=%d capped_units=%d incomplete_units=%d",
		s.InspectedRuns, s.InspectedAttempts, s.InspectedArtifacts, s.RawCases, s.FactualResults, s.FactualResultBearingCommits, s.ReplayDuplicates, s.CappedUnits, s.IncompleteUnits)
	stamp := func(value *time.Time) string {
		if value == nil || value.IsZero() {
			return "unknown"
		}
		return value.UTC().Format(time.RFC3339Nano)
	}
	fmt.Fprintf(&b, " requested_since=%s requested_until=%s observed_since=%s observed_until=%s",
		stamp(&s.RequestedSince), stamp(&s.RequestedUntil), stamp(s.ObservedSince), stamp(s.ObservedUntil))
	reasons := make([]string, 0, len(s.AcquisitionExclusions))
	for reason := range s.AcquisitionExclusions {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(&b, " %s=%d", reason, s.AcquisitionExclusions[reason])
	}
	return b.String()
}

// ProviderObservation is one artifact/report transaction. UnitKey identifies
// the actual provider source even when artifact/job/attempt fields are unknown.
// Evidence strings must be bounded, nonsecret verifier summaries, never logs.
type ProviderObservation struct {
	Provider, Project, RunID, UnitKey, ContentSHA256, RawBlobPath  string
	SourceHeadSHA, Branch, State                                   string
	ArtifactID, JobID, EnvironmentID, TestedSHA                    *string
	Attempt, RunAttempt                                            *int
	TestedRevisionEvidence, ExecutionEvidence, EnvironmentEvidence *string
	// ObservedAt is the source report timestamp supplied by acquisition, not
	// the local mining clock. Exact replay/enrichment preserves the first value.
	ObservedAt                                   time.Time
	RunCreatedAt, RequestedSince, RequestedUntil *time.Time
	SuiteIncomplete, DiagnosticsTruncated        bool
	ExclusionReason, ExclusionDetail             string
	Commit                                       *CommitMetadata
	Entries                                      []ObservationEntry
}

type ObservationEntry struct {
	Name, SHA256 string
	Cases        []ObservationCase
}

type ObservationCase struct {
	Case                             runner.JUnitCase
	Identity                         runner.TestIdentity
	IdentityEvidence                 string
	IdentityError                    string
	ExclusionReason, ExclusionDetail string
	InfraReason                      string
}

// ParentSHAs nil means unknown; an empty nonnil slice is a verified root.
// Kinds nil is JSON null until actual classification is available.
type CommitMetadata struct {
	AuthoredAt                             *time.Time
	ParentSHAs                             []string
	FilesChanged, LinesAdded, LinesRemoved *int
	Kinds                                  map[string]int
}

// AcquisitionConflictError contains a safe reason code, never source values.
type AcquisitionConflictError struct{ Reason string }

func (e *AcquisitionConflictError) Error() string { return "acquisition conflict: " + e.Reason }
