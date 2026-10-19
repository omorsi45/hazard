package mine

import (
	"context"
	"time"
)

type sourceRun struct {
	ID, SourceHeadSHA, Branch, Status string
	CreatedAt                         *time.Time
	RunAttempt                        *int
}

type acquisitionGap struct {
	RunID, UnitKey    string
	JobID, ArtifactID *string
	Attempt           *int
	SourceTime        *time.Time
	Reason, Detail    string
	Raw               []byte
}

type acquiredArtifact struct {
	Observation ProviderObservation
	Raw         []byte
}

type acquisitionSink struct {
	Artifact func(acquiredArtifact) error
	Gap      func(acquisitionGap) error
}

type runOutcome struct {
	State    string
	Attempts []int
}

type acquisitionProvider interface {
	listRuns(context.Context) ([]sourceRun, []acquisitionGap, error)
	acquireRun(context.Context, sourceRun, acquisitionSink) (runOutcome, error)
}
