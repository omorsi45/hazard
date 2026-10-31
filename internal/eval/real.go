package eval

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/omorsi45/hazard/internal/labels"
	"github.com/omorsi45/hazard/internal/store"
)

// RealLabel names the CI population wherever its numbers (or their absence)
// appear.
const RealLabel = "real CI labels"

// Methods of --split.
const (
	SplitTemporal = "temporal"
	SplitRandom   = "random"
)

// DateLayout is the layout of --split-at.
const DateLayout = "2006-01-02"

// SplitSpec says how the strong units of the real population divide into tune
// and test.
type SplitSpec struct {
	Method string
	At     time.Time // temporal: units strictly before it are tune, the others test
}

// UnitSplit is how many strong units fall on each side of the split.
type UnitSplit struct {
	Method string `json:"method"`
	At     string `json:"at,omitempty"`
	Tune   int    `json:"tune"`
	Test   int    `json:"test"`
}

// RealLabels is what the current label build holds, as counts. Nothing here is
// a metric: a count of zero is a measurement, an unavailable build is not.
type RealLabels struct {
	Available          bool           `json:"available"`
	Reason             string         `json:"reason,omitempty"`
	BuildID            string         `json:"build_id,omitempty"`
	Cutoff             string         `json:"cutoff,omitempty"`
	Freshness          string         `json:"freshness,omitempty"`
	RawFailingCases    int            `json:"raw_failing_cases"`
	RawCandidates      int            `json:"raw_candidates"`
	StrongCandidates   int            `json:"strong_candidates"`
	WeakCandidates     int            `json:"weak_candidates"`
	PendingCandidates  int            `json:"pending_candidates"`
	ExcludedCandidates int            `json:"excluded_candidates"`
	StrongUnits        int            `json:"strong_units"`
	Unusable           map[string]int `json:"unusable_strong_commits,omitempty"`
	Split              *UnitSplit     `json:"split,omitempty"`
}

// Real is the section of the report about the CI population.
type Real struct {
	Population string     `json:"population"`
	Status     string     `json:"status"`
	Labels     RealLabels `json:"labels"`
	Rows       []Row      `json:"rows"`
}

// ReadReal reads the counts of the current label build and splits its strong
// units. A database that does not exist, or has no label build, is
// unavailable: it is not an empty population.
func ReadReal(ctx context.Context, dbPath string, split SplitSpec) (_ RealLabels, err error) {
	if _, statErr := os.Stat(dbPath); errors.Is(statErr, fs.ErrNotExist) {
		return RealLabels{Reason: fmt.Sprintf("there is no database at %s: run hazard ci-mine and hazard labels build", dbPath)}, nil
	} else if statErr != nil {
		return RealLabels{}, fmt.Errorf("eval: %w", statErr)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		return RealLabels{}, fmt.Errorf("eval: %w", err)
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	rep, err := labels.ReadStats(ctx, s.DB())
	if err != nil {
		return RealLabels{}, fmt.Errorf("eval: read the label build: %w", err)
	}
	if rep.Freshness == labels.FreshnessUnavailable {
		return RealLabels{Freshness: rep.Freshness, Reason: "there is no current label build: run hazard labels build"}, nil
	}
	units, err := labels.StrongUnits(ctx, s.DB())
	if err != nil {
		return RealLabels{}, fmt.Errorf("eval: read the strong units: %w", err)
	}
	out := RealLabels{
		Available: true, BuildID: rep.BuildID, Cutoff: rep.Cutoff, Freshness: rep.Freshness,
		RawFailingCases: rep.RawFailingCases, RawCandidates: rep.RawCandidates, StrongCandidates: rep.StrongCandidates,
		WeakCandidates: rep.WeakCandidates, PendingCandidates: rep.PendingCandidates, ExcludedCandidates: rep.ExcludedCandidates,
		StrongUnits: len(units.Units), Split: SplitUnits(units.Units, split),
	}
	if len(units.Unusable) > 0 {
		out.Unusable = units.Unusable
	}
	return out, nil
}

// SplitUnits counts the units on each side of the split. Temporal: before
// spec.At is tune, the rest test; without a split point there is nothing to
// count. Random: the first half (rounded down) is tune and the rest test.
func SplitUnits(units []labels.Unit, spec SplitSpec) *UnitSplit {
	switch spec.Method {
	case SplitTemporal:
		if spec.At.IsZero() {
			return nil
		}
		s := &UnitSplit{Method: SplitTemporal, At: spec.At.Format(DateLayout)}
		for _, u := range units {
			if u.CommitTime.Before(spec.At) {
				s.Tune++
			} else {
				s.Test++
			}
		}
		return s
	case SplitRandom:
		return &UnitSplit{Method: SplitRandom, Tune: len(units) / 2, Test: len(units) - len(units)/2}
	}
	return nil
}

// status says in one line what the real population holds and why no number
// is printed for it.
func (l RealLabels) status() string {
	switch {
	case !l.Available:
		return fmt.Sprintf("%s (label data unavailable: %s)", NotAvailable, l.Reason)
	case l.Freshness != labels.FreshnessCurrent:
		return fmt.Sprintf("%s (the label build is %s: %d raw failing cases; run hazard labels build)", NotAvailable, l.Freshness, l.RawFailingCases)
	case l.StrongUnits == 0:
		return fmt.Sprintf("%s (0 strong units, %d raw failing cases)", NotAvailable, l.RawFailingCases)
	}
	return fmt.Sprintf("not evaluated (%d strong units, %d raw failing cases): replaying real CI units is not part of this report", l.StrongUnits, l.RawFailingCases)
}

// BuildReal makes the rows of the CI population. With no strong unit every
// cell reads N/A, because nothing is measured, and the status carries the
// counts. With strong units the rows say they were not evaluated.
func BuildReal(l RealLabels, plan Plan) Real {
	status := l.status()
	real := Real{Population: RealLabel, Status: status, Labels: l}
	empty := !l.Available || l.Freshness != labels.FreshnessCurrent || l.StrongUnits == 0

	add := func(id, predictor string) {
		row := Row{ID: id, Name: rowNames[id], Population: RealLabel, Predictor: predictor, Status: status}
		if empty {
			row.Metrics = Summarize(nil, metricOrder, plan.Reps, plan.Seed)
		}
		real.Rows = append(real.Rows, row)
	}
	if plan.wants(BaselineAll) {
		add(RowB0, "")
	}
	if plan.wants(BaselineL1Only) {
		add(RowB1, "")
	}
	if plan.wants(BaselineL1Coverage) {
		add(RowB2, "")
	}
	for _, ps := range plan.Predictors {
		add(ps.ID, ps.Kind)
	}
	return real
}
