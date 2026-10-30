package events

import (
	"strings"

	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/predict"
)

// ExampleStats counts the failing tests of an event that did not become rows.
type ExampleStats struct {
	FailingCertain  int // already selected with certainty by L1
	FailingUnlisted int // neither CERTAIN nor a residual candidate (cut by the cap, or unknown to the index)
}

// Examples turns the L1 outcome of a replayed event into training rows, one
// per residual candidate: labelled by whether the candidate is among the
// event's failing tests, with the L1 rank and feature vector, the given weight
// and the event's group. A failing test that L1 selected with certainty is not
// a row (the predictor never scores it); it is counted in the stats, as is a
// failing test that is neither CERTAIN nor a candidate.
//
// When L1 is missing or demanded the full suite, or has no candidate left to
// score, there are no rows and skip names the reason: the kind of the full
// suite reason (the text before the first colon), the engine's abstain reason
// when L1 did not run, or "no_candidates".
func Examples(d engine.Decision, ev Event, weight float64) (rows []predict.Example, stats ExampleStats, skip string) {
	switch {
	case d.L1 == nil:
		if d.AbstainReason == "" {
			return nil, ExampleStats{}, "no_l1"
		}
		return nil, ExampleStats{}, reasonKind(d.AbstainReason)
	case d.L1.FullReason != "":
		return nil, ExampleStats{}, reasonKind(d.L1.FullReason)
	case len(d.L1.Candidates) == 0:
		return nil, ExampleStats{}, "no_candidates"
	}
	failing := make(map[string]bool, len(ev.Failing))
	for _, id := range ev.Failing {
		failing[id] = true
	}
	listed := make(map[string]bool, len(d.L1.Candidates))
	rows = make([]predict.Example, 0, len(d.L1.Candidates))
	for _, c := range d.L1.Candidates {
		listed[c.TestID] = true
		rows = append(rows, predict.Example{
			Rank: c.Rank, Features: c.Features.Vector(), Label: failing[c.TestID], Weight: weight, Group: ev.Group,
		})
	}
	certain := make(map[string]bool, len(d.L1.Certain))
	for _, m := range d.L1.Certain {
		certain[m.TestID] = true
	}
	for _, id := range ev.Failing {
		switch {
		case certain[id]:
			stats.FailingCertain++
		case !listed[id]:
			stats.FailingUnlisted++
		}
	}
	return rows, stats, ""
}

// reasonKind is the part of a full-suite reason before its first colon, so that
// reasons carrying a path or a package do not each become their own key.
func reasonKind(reason string) string {
	kind, _, _ := strings.Cut(reason, ":")
	return kind
}
