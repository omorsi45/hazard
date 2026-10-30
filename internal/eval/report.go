package eval

import (
	"maps"
	"slices"
	"strconv"
)

// SchemaVersion is the version of report.json.
const SchemaVersion = 1

// Row identifiers of the ablation (seed 12.2).
const (
	RowB0 = "B0"
	RowB1 = "B1"
	RowB2 = "B2"
	RowP0 = "P0"
	RowP1 = "P1"
	RowP2 = "P2"
	RowP3 = "P3"
)

// StatusMeasured is the Status of a row that has numbers.
const StatusMeasured = "measured"

// Layers a row can be computed in.
const (
	ModeGraph    = "graph"
	ModeCoverage = "coverage"
)

// Baseline names accepted by --baselines.
const (
	BaselineAll        = "all"
	BaselineL1Only     = "l1-only"
	BaselineL1Coverage = "l1-coverage"
)

const (
	noDurations    = NotAvailable + " (no test durations)"
	noCoverageText = "unavailable: the index has no coverage"
)

var rowNames = map[string]string{
	RowB0: "B0 run everything",
	RowB1: "B1 L1 graph only",
	RowB2: "B2 L1 + coverage",
	RowP0: "P0 + static rank",
	RowP1: "P1 + logistic",
	RowP2: "P2 + Jev",
	RowP3: "P3 + ensemble",
}

// Row is one configuration of the ablation on one population. A row whose
// Status is not StatusMeasured has no numbers: every cell reads Status.
type Row struct {
	ID         string                       `json:"id"`
	Name       string                       `json:"name"`
	Population string                       `json:"population"`
	Predictor  string                       `json:"predictor,omitempty"`
	Mode       string                       `json:"l1_mode,omitempty"`
	Status     string                       `json:"status"`
	Fit        *Fit                         `json:"operating_point,omitempty"`
	Note       string                       `json:"note,omitempty"`
	Flag       string                       `json:"recall_flag,omitempty"`
	Metrics    map[string]Metric            `json:"metrics,omitempty"`
	ByKind     map[string]map[string]Metric `json:"by_kind,omitempty"`
}

// Pair is one scored (change, test) pair of the test partition.
type Pair struct {
	Config string
	Event  string
	Test   string
	P      float64
	Label  bool
}

// Reliability is the 10-bin table of one predictor.
type Reliability struct {
	Config     string `json:"config"`
	Predictor  string `json:"predictor"`
	Population string `json:"population"`
	Pairs      int    `json:"pairs"`
	Bins       []Bin  `json:"bins"`
}

// PredictorState says whether a predictor can be scored. Status is empty when
// it can; otherwise it is the text every cell of its row reads.
type PredictorState struct {
	Kind   string
	ID     string
	Status string
}

// Plan is what BuildSynthetic evaluates.
type Plan struct {
	Baselines   []string // all, l1-only, l1-coverage
	Predictors  []PredictorState
	Grid        []float64
	Recall      float64
	Reps        int
	Seed        uint64
	HasCoverage bool
	PMode       string // the L1 mode the predictors scored: graph or coverage
	Cost        *CostModel
}

// Built is the measured part of the synthetic section.
type Built struct {
	Rows        []Row
	Reliability []Reliability
	Pairs       []Pair
}

// recallText renders a recall target the way the labels print it: 0.99, 0.95.
func recallText(recall float64) string { return strconv.FormatFloat(recall, 'f', -1, 64) }

// names are the metrics computed for a plan: the time metrics need durations.
func (p Plan) names() []string {
	names := []string{
		MetricRecallCommit, MetricRecallFailure, MetricSelectionRatio, MetricAbstention,
		MetricBrier, MetricTruncation, MetricUSD, MetricLatency,
	}
	if p.Cost != nil {
		names = append(names, MetricTimeRatio, MetricSaving)
	}
	return names
}

// summarize computes the cells of one row from its outcomes; the time cells of
// a plan without durations read N/A.
func (p Plan) summarize(id string, outcomes []Outcome) map[string]Metric {
	m := Summarize(outcomes, p.names(), p.Reps, p.Seed)
	if p.Cost == nil {
		m[MetricTimeRatio] = Metric{NA: noDurations}
		m[MetricSaving] = Metric{NA: noDurations}
	}
	switch id {
	case RowB0:
		m[MetricAbstention] = Metric{N: len(outcomes), NA: NotAvailable} // it never abstains: not applicable
	case RowB1, RowB2:
		m[MetricTruncation] = Metric{N: len(outcomes), NA: NotAvailable} // no candidate list, no cap to miss through
	}
	return m
}

// byKind is the breakdown of a row by the change kind of the event.
func (p Plan) byKind(id string, outcomes []Outcome) map[string]map[string]Metric {
	groups := map[string][]Outcome{}
	for _, o := range outcomes {
		groups[o.Kind] = append(groups[o.Kind], o)
	}
	out := make(map[string]map[string]Metric, len(groups))
	for _, kind := range slices.Sorted(maps.Keys(groups)) {
		out[kind] = p.summarize(id, groups[kind])
	}
	return out
}

// measured is a row with numbers.
func (p Plan) measured(id, predictor, mode string, outcomes []Outcome) Row {
	r := Row{
		ID: id, Name: rowNames[id], Population: SyntheticLabel, Predictor: predictor, Mode: mode, Status: StatusMeasured,
		Metrics: p.summarize(id, outcomes), ByKind: p.byKind(id, outcomes),
	}
	if v := r.Metrics[MetricRecallCommit].Value; v != nil && *v < p.Recall {
		r.Flag = "below " + recallText(p.Recall)
	}
	return r
}

// unmeasured is a row with no numbers: every cell reads status.
func unmeasured(id, predictor, status string) Row {
	return Row{ID: id, Name: rowNames[id], Population: SyntheticLabel, Predictor: predictor, Status: status}
}

func (p Plan) wants(baseline string) bool { return slices.Contains(p.Baselines, baseline) }

// BuildSynthetic evaluates every configuration of the plan on the test events,
// with lambda fitted on the tune events only (the test events are never read
// to choose it).
func BuildSynthetic(plan Plan, tune, test []*Event) Built {
	ev := Evaluator{Cost: plan.Cost}
	var b Built

	outcomes := func(f func(*Event) Outcome) []Outcome {
		out := make([]Outcome, len(test))
		for i, e := range test {
			out[i] = f(e)
		}
		return out
	}
	if plan.wants(BaselineAll) {
		b.Rows = append(b.Rows, plan.measured(RowB0, "", "", outcomes(ev.Full)))
	}
	if plan.wants(BaselineL1Only) {
		b.Rows = append(b.Rows, plan.measured(RowB1, "", ModeGraph, outcomes(func(e *Event) Outcome { return ev.Baseline(e, e.Graph) })))
	}
	if plan.wants(BaselineL1Coverage) {
		if plan.HasCoverage {
			b.Rows = append(b.Rows, plan.measured(RowB2, "", ModeCoverage, outcomes(func(e *Event) Outcome { return ev.Baseline(e, e.Coverage) })))
		} else {
			b.Rows = append(b.Rows, unmeasured(RowB2, "", noCoverageText))
		}
	}

	for _, ps := range plan.Predictors {
		if ps.Status != "" {
			b.Rows = append(b.Rows, unmeasured(ps.ID, ps.Kind, ps.Status))
			continue
		}
		res := ev.RunP(tune, test, ps.Kind, plan.Grid, plan.Recall)
		if res.Fit.Reason != "" {
			fit := res.Fit
			row := unmeasured(ps.ID, ps.Kind, NotAvailable+" ("+fit.Reason+")")
			row.Fit = &fit
			b.Rows = append(b.Rows, row)
			continue
		}
		row := plan.measured(ps.ID, ps.Kind, plan.PMode, res.Outcomes)
		fit := res.Fit
		row.Fit = &fit
		if !fit.Feasible {
			row.Note = "infeasible at R_c >= " + recallText(plan.Recall)
		}
		b.Rows = append(b.Rows, row)

		var labels []ProbLabel
		for _, e := range test {
			for _, pl := range e.Pairs(ps.Kind) {
				labels = append(labels, pl)
				b.Pairs = append(b.Pairs, Pair{Config: ps.ID, Event: e.ID, Test: pl.Test, P: pl.P, Label: pl.Label})
			}
		}
		b.Reliability = append(b.Reliability, Reliability{
			Config: ps.ID, Predictor: ps.Kind, Population: SyntheticLabel, Pairs: len(labels), Bins: ReliabilityBins(labels),
		})
	}
	return b
}
