package predict

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/impact"
)

// TrainingSchema is the layout version of training.json.
const TrainingSchema = 1

// TrainEvent is the training rows of one source event: a mutant or a CI
// commit. Every row of an event carries the event's Group.
type TrainEvent struct {
	ID, Origin, Group string
	Time              time.Time
	Examples          []Example
}

// EventCount is the number of observed events with one origin, partition and
// status.
type EventCount struct {
	Origin    string `json:"origin"`
	Partition string `json:"partition"`
	Status    string `json:"status"`
	Count     int    `json:"count"`
}

// Collected is what the corpus builder reports besides the events it turned
// into rows: every event it observed, why some yielded no rows, and the failing
// tests that could not become rows.
type Collected struct {
	Events          []EventCount   `json:"events"`
	Skipped         map[string]int `json:"skipped"`
	FailingCertain  int            `json:"failing_certain"`  // failing tests L1 already selected with certainty
	FailingUnlisted int            `json:"failing_unlisted"` // failing tests that were neither CERTAIN nor candidates
}

// ExampleCounts totals events and rows.
type ExampleCounts struct {
	Events    int `json:"events"`
	Examples  int `json:"examples"`
	Positives int `json:"positives"`
	Negatives int `json:"negatives"`
}

// SplitReport describes one side of the group split.
type SplitReport struct {
	Groups    []string `json:"groups"`    // oldest first
	EventIDs  []string `json:"event_ids"` // sorted
	Events    int      `json:"events"`
	Examples  int      `json:"examples"`
	Positives int      `json:"positives"`
	Negatives int      `json:"negatives"`
}

// Refusal is one fit that was not made, and why.
type Refusal struct {
	Predictor string `json:"predictor"`
	Reason    string `json:"reason"`
}

// ArtifactReport locates an artifact that was written.
type ArtifactReport struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// TrainingManifest is training.json: the audit record of one training run.
type TrainingManifest struct {
	Schema         int                       `json:"schema"`
	Cutoff         string                    `json:"cutoff"`
	CreatedAt      string                    `json:"created_at"`
	FeatureVersion string                    `json:"feature_version"`
	Options        LogisticOptions           `json:"options"`
	Collected      Collected                 `json:"collected"`
	Examples       ExampleCounts             `json:"examples"`
	Splits         map[string]SplitReport    `json:"splits"`
	Refusals       []Refusal                 `json:"refusals"`
	Artifacts      map[string]ArtifactReport `json:"artifacts"`
}

// TrainInput is everything Train needs.
type TrainInput struct {
	Cutoff    string // YYYY-MM-DD, the exclusive UTC cutoff the events were restricted to
	Events    []TrainEvent
	Collected Collected
	Options   LogisticOptions // zero value: DefaultLogisticOptions
	OutDir    string
	Now       func() time.Time // nil: time.Now
}

// fitFraction is the share of groups, oldest first, that fits the models; the
// newest share scales. It is fixed before any evaluation.
const fitFraction = 0.8

// side is one side of the split: the events, their groups and rows.
type side struct {
	groups   []string // oldest first
	events   []TrainEvent
	examples []Example
}

func (s side) report() SplitReport {
	r := SplitReport{Groups: append([]string{}, s.groups...), EventIDs: []string{}, Events: len(s.events), Examples: len(s.examples)}
	for _, ev := range s.events {
		r.EventIDs = append(r.EventIDs, ev.ID)
	}
	sort.Strings(r.EventIDs)
	r.Positives, r.Negatives = classCounts(s.examples)
	return r
}

func classCounts(ex []Example) (pos, neg int) {
	for _, e := range ex {
		if e.Label {
			pos++
		} else {
			neg++
		}
	}
	return pos, neg
}

// classSupport refuses a set that cannot support a fit: a model fitted on one
// class only would invent probabilities.
func classSupport(name string, ex []Example) error {
	pos, neg := classCounts(ex)
	switch {
	case len(ex) == 0:
		return fmt.Errorf("%s set has no examples", name)
	case pos == 0:
		return fmt.Errorf("%s set has no positive examples (%d negatives)", name, neg)
	case neg == 0:
		return fmt.Errorf("%s set has no negative examples (%d positives)", name, pos)
	}
	return nil
}

// Train splits the events by group into a chronological fit set (the oldest
// 80% of the groups) and a scale set (the newest 20%), fits P0 on the fit set
// and P1 on both, and writes static.json, logistic.json and training.json to
// in.OutDir. A group is placed by the time of its latest event, equal times by
// group name. A fit whose sets lack either class, or whose descent does not
// converge, is refused and recorded in Refusals; the other fit still stands.
// When both are refused nothing is written. An existing artifact of a refused
// fit is left untouched. The error is non-nil only when writing fails.
func Train(in TrainInput) (TrainingManifest, error) {
	opts := in.Options
	if opts == (LogisticOptions{}) {
		opts = DefaultLogisticOptions()
	}
	now := in.Now
	if now == nil {
		now = time.Now
	}
	created := now().UTC().Format(time.RFC3339)
	m := TrainingManifest{
		Schema: TrainingSchema, Cutoff: in.Cutoff, CreatedAt: created, FeatureVersion: impact.FeatureVersion, Options: opts,
		Collected: in.Collected, Refusals: []Refusal{}, Artifacts: map[string]ArtifactReport{},
	}
	if m.Collected.Events == nil {
		m.Collected.Events = []EventCount{}
	}
	if m.Collected.Skipped == nil {
		m.Collected.Skipped = map[string]int{}
	}

	fit, scale := splitEvents(in.Events)
	m.Splits = map[string]SplitReport{"fit": fit.report(), "scale": scale.report()}
	m.Examples = ExampleCounts{Events: len(fit.events) + len(scale.events), Examples: len(fit.examples) + len(scale.examples)}
	m.Examples.Positives, m.Examples.Negatives = classCounts(append(append([]Example{}, fit.examples...), scale.examples...))

	var static *StaticArtifact
	var logistic *LogisticArtifact
	if a, err := fitStatic(fit, in.Cutoff, created, opts); err != nil {
		m.Refusals = append(m.Refusals, Refusal{Predictor: "static", Reason: err.Error()})
	} else {
		static = &a
	}
	if a, err := fitLogistic(fit, scale, in.Cutoff, created, opts); err != nil {
		m.Refusals = append(m.Refusals, Refusal{Predictor: "logistic", Reason: err.Error()})
	} else {
		logistic = &a
	}
	if static == nil && logistic == nil {
		return m, nil
	}

	if err := os.MkdirAll(in.OutDir, 0o755); err != nil {
		return m, fmt.Errorf("train: create the output directory: %w", err)
	}
	if static != nil {
		path := filepath.Join(in.OutDir, "static.json")
		if err := WriteStatic(path, *static); err != nil {
			return m, fmt.Errorf("train: %w", err)
		}
		written, err := ReadStatic(path)
		if err != nil {
			return m, fmt.Errorf("train: verify what was written: %w", err)
		}
		m.Artifacts["static"] = ArtifactReport{Path: path, Digest: written.Digest}
	}
	if logistic != nil {
		path := filepath.Join(in.OutDir, "logistic.json")
		if err := WriteLogistic(path, *logistic); err != nil {
			return m, fmt.Errorf("train: %w", err)
		}
		written, err := ReadLogistic(path)
		if err != nil {
			return m, fmt.Errorf("train: verify what was written: %w", err)
		}
		m.Artifacts["logistic"] = ArtifactReport{Path: path, Digest: written.Digest}
	}
	if err := writeJSON(filepath.Join(in.OutDir, "training.json"), m); err != nil {
		return m, fmt.Errorf("train: %w", err)
	}
	return m, nil
}

// splitEvents orders the groups of the events that have rows by the time of
// their latest event, then by name, and gives the oldest 80% to the fit side.
func splitEvents(events []TrainEvent) (fit, scale side) {
	latest := map[string]time.Time{}
	var withRows []TrainEvent
	for _, ev := range events {
		if len(ev.Examples) == 0 {
			continue
		}
		withRows = append(withRows, ev)
		if cur, ok := latest[ev.Group]; !ok || ev.Time.After(cur) {
			latest[ev.Group] = ev.Time
		}
	}
	keyOf := make(map[string]string, len(latest))
	keys := make([]string, 0, len(latest))
	for group, at := range latest {
		key := at.UTC().Format("2006-01-02T15:04:05Z") + "|" + group
		keyOf[group] = key
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fitKeys, _ := SplitGroups(keys, fitFraction)
	for _, key := range keys {
		_, group, _ := strings.Cut(key, "|")
		if fitKeys[key] {
			fit.groups = append(fit.groups, group)
		} else {
			scale.groups = append(scale.groups, group)
		}
	}
	sort.SliceStable(withRows, func(i, j int) bool { return withRows[i].ID < withRows[j].ID })
	for _, ev := range withRows {
		s := &scale
		if fitKeys[keyOf[ev.Group]] {
			s = &fit
		}
		s.events = append(s.events, ev)
		s.examples = append(s.examples, ev.Examples...)
	}
	return fit, scale
}

// trainingInfo is the provenance recorded in an artifact fitted on sides.
func trainingInfo(cutoff, created string, opts LogisticOptions, fit, scale side) TrainingInfo {
	sources := map[string]int{}
	for _, s := range []side{fit, scale} {
		for _, ev := range s.events {
			sources[ev.Origin]++
		}
	}
	pos, neg := classCounts(append(append([]Example{}, fit.examples...), scale.examples...))
	return TrainingInfo{
		Cutoff: cutoff, FitGroups: append([]string{}, fit.groups...), ScaleGroups: append([]string{}, scale.groups...),
		Positives: pos, Negatives: neg, Sources: sources, CreatedAt: created, Options: opts,
	}
}

// fitStatic fits P0 on the fit side only.
func fitStatic(fit side, cutoff, created string, opts LogisticOptions) (StaticArtifact, error) {
	if err := classSupport("fit", fit.examples); err != nil {
		return StaticArtifact{}, err
	}
	xs, ys, ws := make([]float64, len(fit.examples)), make([]float64, len(fit.examples)), make([]float64, len(fit.examples))
	for i, e := range fit.examples {
		xs[i], ws[i] = e.Rank, e.Weight
		if e.Label {
			ys[i] = 1
		}
	}
	model, err := FitIsotonic(xs, ys, ws)
	if err != nil {
		return StaticArtifact{}, fmt.Errorf("the isotonic fit failed: %w", err)
	}
	return StaticArtifact{
		Schema: ArtifactSchema, FeatureVersion: impact.FeatureVersion, Model: model,
		Training: trainingInfo(cutoff, created, opts, fit, side{}),
	}, nil
}

// fitLogistic fits P1 on the fit side and Platt scaling on the scale side.
func fitLogistic(fit, scale side, cutoff, created string, opts LogisticOptions) (LogisticArtifact, error) {
	if err := classSupport("fit", fit.examples); err != nil {
		return LogisticArtifact{}, err
	}
	if err := classSupport("scale", scale.examples); err != nil {
		return LogisticArtifact{}, err
	}
	model, err := FitLogistic(fit.examples, scale.examples, opts)
	if err != nil {
		return LogisticArtifact{}, fmt.Errorf("the logistic fit failed: %w", err)
	}
	if !model.Converged {
		return LogisticArtifact{}, fmt.Errorf("the fit did not converge (%d iterations of at most %d, gradient tolerance %g)", model.Iterations, opts.MaxIter, opts.Tol)
	}
	return LogisticArtifact{
		Schema: ArtifactSchema, FeatureVersion: impact.FeatureVersion, Model: model,
		Training: trainingInfo(cutoff, created, opts, fit, scale),
	}, nil
}
