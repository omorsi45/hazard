package eval

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Names of the files a run writes next to each other.
const (
	FileReport      = "report.json"
	FilePairs       = "pairs.csv"
	FileHTML        = "report.html"
	FileReliability = "reliability.csv"
)

// Files names the artifacts of a run.
type Files struct {
	Report      string `json:"report"`
	Pairs       string `json:"pairs,omitempty"`
	HTML        string `json:"html"`
	Reliability string `json:"reliability,omitempty"`
}

// DurationsInfo says what the cost model was built from.
type DurationsInfo struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Path      string `json:"path,omitempty"`
	BaseSHA   string `json:"base_sha,omitempty"`
	Known     int    `json:"known_tests"`
	Imputed   int    `json:"imputed_tests"`
	Ignored   int    `json:"ignored_entries"`
}

// Inputs are the settings and sources a report was computed from.
type Inputs struct {
	Source     string          `json:"source"`
	Split      string          `json:"split"`
	SplitAt    string          `json:"split_at,omitempty"`
	Recall     float64         `json:"recall"`
	Bootstrap  int             `json:"bootstrap"`
	Seed       uint64          `json:"seed"`
	Grid       string          `json:"grid"`
	Baselines  []string        `json:"baselines"`
	Predictors []PredictorInfo `json:"predictors"`
	Durations  DurationsInfo   `json:"durations"`
}

// Synthetic is the section of the report about the mutation population.
type Synthetic struct {
	Population   string         `json:"population"`
	MutationBase string         `json:"mutation_base,omitempty"`
	PMode        string         `json:"p_mode,omitempty"`
	Observed     []Count        `json:"observed_mutants"`
	Skipped      map[string]int `json:"not_events"`
	Events       EventCounts    `json:"events"`
	Rows         []Row          `json:"rows"`
	Reliability  []Reliability  `json:"reliability"`
	PairsFile    string         `json:"pairs_file"`
}

// Report is report.json.
type Report struct {
	Schema    int        `json:"schema_version"`
	Tool      string     `json:"tool"`
	Inputs    Inputs     `json:"inputs"`
	Synthetic *Synthetic `json:"synthetic,omitempty"`
	Real      *Real      `json:"real,omitempty"`
	Files     Files      `json:"files"`
}

// column is one metric column of the ablation tables.
type column struct{ Metric, Head string }

var columns = []column{
	{MetricRecallCommit, "R_c"}, {MetricRecallFailure, "R_f"}, {MetricSelectionRatio, "selection ratio"},
	{MetricTimeRatio, "time ratio"}, {MetricAbstention, "abstention"}, {MetricSaving, "effective saving"},
	{MetricBrier, "Brier"}, {MetricUSD, "$/commit"}, {MetricLatency, "latency p95 (ms)"}, {MetricTruncation, "truncation miss"},
}

// cell is the text of one metric: the value with its interval, or why there is
// none.
func cell(name string, m Metric) string {
	if m.Value == nil {
		if m.NA == "" {
			return NotAvailable
		}
		return m.NA
	}
	switch name {
	case MetricLatency:
		return fmt.Sprintf("%.1f", *m.Value)
	case MetricUSD:
		return fmt.Sprintf("$%.4f", *m.Value)
	}
	s := fmt.Sprintf("%.3f", *m.Value)
	if m.Low != nil && m.High != nil {
		s += fmt.Sprintf(" [%.3f, %.3f]", *m.Low, *m.High)
	}
	return s
}

// lambdaCell is the operating point of a row, or a dash when it has none.
func lambdaCell(r Row) string {
	if r.Fit == nil || r.Fit.Reason != "" {
		return "-"
	}
	return strconv.FormatFloat(r.Fit.Lambda, 'f', -1, 64)
}

// notes are the remarks on a row: where it could not reach the target, and
// the counts of a population with nothing to measure.
func notes(r Row) string {
	var parts []string
	if r.Note != "" {
		parts = append(parts, r.Note)
	}
	if r.Flag != "" {
		parts = append(parts, r.Flag)
	}
	if r.Status != StatusMeasured && len(r.Metrics) > 0 {
		parts = append(parts, r.Status) // an empty population: every cell is N/A and the counts follow
	}
	return strings.Join(parts, "; ")
}

// tableRow is one line of a table: the lead cells, then either the metric
// cells or one text that spans them.
type tableRow struct {
	Lead  []string
	Cells []string
	Span  string
	Note  string
}

// metricCells is the row of cells for a metrics map.
func metricCells(metrics map[string]Metric) []string {
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = cell(c.Metric, metrics[c.Metric])
	}
	return out
}

// rowsOf makes the lines of the ablation table of a section.
func rowsOf(rows []Row) []tableRow {
	out := make([]tableRow, len(rows))
	for i, r := range rows {
		tr := tableRow{Lead: []string{r.Name, lambdaCell(r)}, Note: notes(r)}
		if len(r.Metrics) == 0 {
			tr.Span = r.Status
		} else {
			tr.Cells = metricCells(r.Metrics)
		}
		out[i] = tr
	}
	return out
}

// kindRows makes the lines of the breakdown by change kind.
func kindRows(rows []Row) []tableRow {
	var out []tableRow
	for _, r := range rows {
		for _, kind := range slices.Sorted(keysOf(r.ByKind)) {
			out = append(out, tableRow{Lead: []string{r.Name, kind}, Cells: metricCells(r.ByKind[kind])})
		}
	}
	return out
}

func keysOf[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// heads of the tables.
func ablationHead() []string {
	head := []string{"configuration", "lambda"}
	for _, c := range columns {
		head = append(head, c.Head)
	}
	return append(head, "note")
}

func kindHead() []string {
	head := []string{"configuration", "change kind"}
	for _, c := range columns {
		head = append(head, c.Head)
	}
	return head
}

var binHead = []string{"bin", "range", "pairs", "mean P", "observed rate"}

// binRows make the lines of a reliability table.
func binRows(bins []Bin) [][]string {
	out := make([][]string, len(bins))
	for i, b := range bins {
		mean, rate := NotAvailable, NotAvailable
		if b.MeanP != nil && b.Observed != nil {
			mean, rate = fmt.Sprintf("%.3f", *b.MeanP), fmt.Sprintf("%.3f", *b.Observed)
		}
		out[i] = []string{strconv.Itoa(b.Index), fmt.Sprintf("[%.1f, %.1f)", b.Lo, b.Hi), strconv.Itoa(b.Count), mean, rate}
	}
	return out
}

// Captions. Every table of the mutation population says what it is made of.
func (s *Synthetic) ablationCaption(in Inputs) string {
	return fmt.Sprintf("Ablation, %s: %d test events (%d with a failing test), lambda fitted on %d tune events (%d with a failing test), recall target %s; fractions of 1 with 95%% bootstrap intervals (%d resamples, seed %d)",
		SyntheticLabel, s.Events.Test, s.Events.TestWithFailures, s.Events.Tune, s.Events.TuneWithFailures, recallText(in.Recall), in.Bootstrap, in.Seed)
}

func (s *Synthetic) kindCaption() string {
	return fmt.Sprintf("By change kind, %s: Go mutants (%d test events) and configuration mutants (%d), lambda as above", SyntheticLabel, s.Events.TestByKind[KindGo], s.Events.TestByKind[KindCfg])
}

func reliabilityCaption(r Reliability) string {
	return fmt.Sprintf("Reliability of %s (%s), %s: %d scored (change, test) pairs of the test events in 10 equal-width bins", r.Config, r.Predictor, SyntheticLabel, r.Pairs)
}

func (r *Real) caption() string { return "Real CI labels: " + r.Status }

// JSON is the report as indented JSON.
func (r Report) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("eval: encode the report: %w", err)
	}
	return buf.Bytes(), nil
}

// textTable prints rows with tab-aligned columns.
func textTable(w io.Writer, head []string, rows []tableRow, notesColumn bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(head, "\t")); err != nil {
		return err
	}
	for _, r := range rows {
		cells := slices.Clone(r.Lead)
		if r.Span != "" {
			cells = append(cells, r.Span)
		} else {
			cells = append(cells, r.Cells...)
			if notesColumn {
				cells = append(cells, r.Note)
			}
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// WriteText prints the tables of the report.
func (r Report) WriteText(w io.Writer) error {
	if s := r.Synthetic; s != nil {
		if _, err := fmt.Fprintf(w, "%s\n", s.ablationCaption(r.Inputs)); err != nil {
			return err
		}
		if err := textTable(w, ablationHead(), rowsOf(s.Rows), true); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "\n%s\n", s.kindCaption()); err != nil {
			return err
		}
		if err := textTable(w, kindHead(), kindRows(s.Rows), false); err != nil {
			return err
		}
		for _, rel := range s.Reliability {
			if _, err := fmt.Fprintf(w, "\n%s\n", reliabilityCaption(rel)); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(tw, strings.Join(binHead, "\t")); err != nil {
				return err
			}
			for _, row := range binRows(rel.Bins) {
				if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
					return err
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if re := r.Real; re != nil {
		if _, err := fmt.Fprintf(w, "%s\n", re.caption()); err != nil {
			return err
		}
		if err := textTable(w, ablationHead(), rowsOf(re.Rows), true); err != nil {
			return err
		}
	}
	return nil
}

// WritePairs writes pairs.csv: one line per scored (change, test) pair of the
// test events. The first line is a comment naming the population.
func WritePairs(w io.Writer, pairs []Pair) error {
	if _, err := io.WriteString(w, "# "+SyntheticLabel+"\n"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"event", "test", "p", "label", "config"}); err != nil {
		return err
	}
	for _, p := range pairs {
		label := "0"
		if p.Label {
			label = "1"
		}
		if err := cw.Write([]string{p.Event, p.Test, strconv.FormatFloat(p.P, 'g', -1, 64), label, p.Config}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteReliability writes reliability.csv: the bins of every predictor. An
// empty bin has no mean and no rate: the fields are empty, never 0.
func WriteReliability(w io.Writer, rel []Reliability) error {
	if _, err := io.WriteString(w, "# "+SyntheticLabel+"\n"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"config", "bin", "lo", "hi", "count", "mean_p", "observed_rate"}); err != nil {
		return err
	}
	g := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	for _, r := range rel {
		for _, b := range r.Bins {
			mean, rate := "", ""
			if b.MeanP != nil && b.Observed != nil {
				mean, rate = g(*b.MeanP), g(*b.Observed)
			}
			if err := cw.Write([]string{r.Config, strconv.Itoa(b.Index), g(b.Lo), g(b.Hi), strconv.Itoa(b.Count), mean, rate}); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// ---- HTML ----

type htmlTableView struct {
	Caption string
	Head    []string
	Rows    []tableRow
	Cols    int  // columns a spanning text covers
	NoteCol bool // the table has a column of notes
}

type htmlPoint struct{ X, Y, R, Tip string }

type htmlDiagram struct {
	ID, Title string
	Points    []htmlPoint
	Ticks     []htmlTick
}

type htmlTick struct{ Label, X, Y string }

type htmlReliability struct {
	Diagram htmlDiagram
	Table   htmlTableView
	Bins    [][]string
}

type htmlKV struct{ Key, Value string }

type htmlView struct {
	Title       string
	Lead        string
	Tables      []htmlTableView
	Reliability []htmlReliability
	Real        *htmlTableView
	Provenance  []htmlKV
}

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body { font-family: system-ui, sans-serif; margin: 2rem auto; max-width: 78rem; padding: 0 1rem; color: #1b1f24; background: #ffffff; }
table { border-collapse: collapse; margin: 1rem 0 2rem; font-size: 0.9rem; }
caption { text-align: left; font-weight: 600; padding-bottom: 0.4rem; }
th, td { border: 1px solid #c8ccd0; padding: 0.25rem 0.5rem; text-align: right; }
th[scope="row"], thead th:first-child { text-align: left; }
td.span { text-align: left; font-style: italic; }
.banner { border-left: 4px solid #9a6700; background: #fff8c5; padding: 0.5rem 1rem; }
svg { max-width: 100%; height: auto; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 0.2rem 1rem; }
dt { font-weight: 600; }
dd { margin: 0; }
</style>
</head>
<body>
<h1>{{.Title}}</h1>
<p class="banner">{{.Lead}}</p>
{{range .Tables}}{{template "table" .}}{{end}}
{{if .Reliability}}<h2>Reliability</h2>
{{range .Reliability}}{{template "diagram" .Diagram}}{{template "bins" .}}{{end}}{{end}}
{{if .Real}}<h2>Real CI labels</h2>
{{template "table" .Real}}{{end}}
<h2>Data provenance</h2>
<dl>
{{range .Provenance}}<dt>{{.Key}}</dt><dd>{{.Value}}</dd>
{{end}}</dl>
</body>
</html>
{{define "table"}}<table>
<caption>{{.Caption}}</caption>
<thead><tr>{{range .Head}}<th scope="col">{{.}}</th>{{end}}</tr></thead>
<tbody>
{{$cols := .Cols}}{{range .Rows}}<tr><th scope="row">{{index .Lead 0}}</th>{{range slice .Lead 1}}<td>{{.}}</td>{{end}}{{if .Span}}<td class="span" colspan="{{$cols}}">{{.Span}}</td>{{else}}{{range .Cells}}<td>{{.}}</td>{{end}}{{if $.NoteCol}}<td>{{.Note}}</td>{{end}}{{end}}</tr>
{{end}}</tbody>
</table>
{{end}}
{{define "diagram"}}<svg viewBox="0 0 340 330" width="340" height="330" role="img" aria-labelledby="{{.ID}}-title">
<title id="{{.ID}}-title">{{.Title}}</title>
<rect x="40" y="10" width="260" height="260" fill="none" stroke="#444444"></rect>
<line x1="40" y1="270" x2="300" y2="10" stroke="#8c959f" stroke-dasharray="4 3"></line>
{{range .Ticks}}<text x="{{.X}}" y="{{.Y}}" font-size="10" text-anchor="middle" fill="#1b1f24">{{.Label}}</text>
{{end}}<text x="170" y="320" font-size="11" text-anchor="middle" fill="#1b1f24">mean predicted probability</text>
<text x="12" y="140" font-size="11" text-anchor="middle" fill="#1b1f24" transform="rotate(-90 12 140)">observed rate</text>
{{range .Points}}<circle cx="{{.X}}" cy="{{.Y}}" r="{{.R}}" fill="#0969da" fill-opacity="0.75"><title>{{.Tip}}</title></circle>
{{end}}</svg>
{{end}}
{{define "bins"}}<table>
<caption>{{.Table.Caption}}</caption>
<thead><tr>{{range .Table.Head}}<th scope="col">{{.}}</th>{{end}}</tr></thead>
<tbody>
{{range .Bins}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>
{{end}}</tbody>
</table>
{{end}}`

var page = template.Must(template.New("page").Parse(pageTemplate))

// diagram is the reliability diagram of one predictor: a point per non-empty
// bin at (mean predicted probability, observed rate), sized by its pairs.
func diagram(r Reliability) htmlDiagram {
	d := htmlDiagram{ID: "reliability-" + strings.ToLower(r.Config), Title: "Reliability diagram of " + r.Config + ", " + SyntheticLabel}
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
	for i := 0; i <= 10; i += 2 {
		v := float64(i) / 10
		d.Ticks = append(d.Ticks,
			htmlTick{Label: fmt.Sprintf("%.1f", v), X: f(40 + 260*v), Y: "286"},
			htmlTick{Label: fmt.Sprintf("%.1f", v), X: "32", Y: f(274 - 260*v)})
	}
	most := 1
	for _, b := range r.Bins {
		most = max(most, b.Count)
	}
	for _, b := range r.Bins {
		if b.MeanP == nil || b.Observed == nil {
			continue
		}
		d.Points = append(d.Points, htmlPoint{
			X: f(40 + 260**b.MeanP), Y: f(270 - 260**b.Observed), R: f(3 + 7*math.Sqrt(float64(b.Count)/float64(most))),
			Tip: fmt.Sprintf("bin %d: %d pairs, mean P %.3f, observed rate %.3f", b.Index, b.Count, *b.MeanP, *b.Observed),
		})
	}
	return d
}

// HTML is the static report page: no script, no external asset, the reliability
// diagrams inline as SVG.
func (r Report) HTML() ([]byte, error) {
	v := htmlView{}
	head := ablationHead()
	switch {
	case r.Synthetic != nil && r.Real != nil:
		v.Title = "Hazard eval: " + SyntheticLabel + " and real CI labels"
	case r.Synthetic != nil:
		v.Title = "Hazard eval: " + SyntheticLabel
	default:
		v.Title = "Hazard eval: real CI labels"
	}
	if s := r.Synthetic; s != nil {
		v.Lead = "The measured tables are on " + SyntheticLabel + ": seeded faults replayed on the repository, scored on the held-out test partition. They say how well each configuration finds the tests a seeded fault breaks. They are not the recall on real regressions."
		v.Tables = append(v.Tables,
			htmlTableView{Caption: s.ablationCaption(r.Inputs), Head: head, Rows: rowsOf(s.Rows), Cols: len(columns) + 1, NoteCol: true},
			htmlTableView{Caption: s.kindCaption(), Head: kindHead(), Rows: kindRows(s.Rows), Cols: len(columns)})
		for _, rel := range s.Reliability {
			v.Reliability = append(v.Reliability, htmlReliability{
				Diagram: diagram(rel), Table: htmlTableView{Caption: reliabilityCaption(rel), Head: binHead}, Bins: binRows(rel.Bins),
			})
		}
	} else {
		v.Lead = "Only the real CI population was asked for."
	}
	if re := r.Real; re != nil {
		v.Real = &htmlTableView{Caption: re.caption(), Head: head, Rows: rowsOf(re.Rows), Cols: len(columns) + 1, NoteCol: true}
		if r.Synthetic != nil {
			v.Lead += " Real CI labels are in the last table: with no strong label there is nothing to measure and every cell reads N/A."
		}
	}
	v.Provenance = r.provenance()
	var buf bytes.Buffer
	if err := page.Execute(&buf, v); err != nil {
		return nil, fmt.Errorf("eval: render the page: %w", err)
	}
	return buf.Bytes(), nil
}

// provenance lists where the numbers came from.
func (r Report) provenance() []htmlKV {
	in := r.Inputs
	kv := []htmlKV{
		{"tool", r.Tool},
		{"schema version", strconv.Itoa(r.Schema)},
		{"source", in.Source},
		{"split", in.Split + orDash(in.SplitAt, " at ")},
		{"recall target", recallText(in.Recall)},
		{"bootstrap", fmt.Sprintf("%d resamples of the events, seed %d, percentile 95%% intervals", in.Bootstrap, in.Seed)},
		{"lambda grid", in.Grid},
		{"baselines", strings.Join(in.Baselines, ", ")},
	}
	d := in.Durations
	if d.Available {
		kv = append(kv, htmlKV{"test durations", fmt.Sprintf("%s (base %s): %d measured tests, %d priced at the median, %d entries ignored", d.Path, d.BaseSHA, d.Known, d.Imputed, d.Ignored)})
	} else {
		kv = append(kv, htmlKV{"test durations", NotAvailable + " (" + d.Reason + ")"})
	}
	for _, p := range in.Predictors {
		kv = append(kv, htmlKV{"predictor " + p.ID + " " + p.Kind, strings.TrimSpace(p.Status + " " + p.ModelRelease + " " + p.WeightsDigest)})
	}
	if s := r.Synthetic; s != nil {
		kv = append(kv,
			htmlKV{"mutation population", SyntheticLabel},
			htmlKV{"mutation base", s.MutationBase},
			htmlKV{"layer the predictors scored", s.PMode},
			htmlKV{"events", fmt.Sprintf("tune %d, test %d, train %d not read", s.Events.Tune, s.Events.Test, s.Events.TrainNotRead)})
		for _, c := range s.Observed {
			kv = append(kv, htmlKV{fmt.Sprintf("mutants %s / %s", c.Partition, c.Status), strconv.Itoa(c.N)})
		}
		kv = append(kv, htmlKV{"scored pairs", s.PairsFile})
	}
	if re := r.Real; re != nil {
		l := re.Labels
		kv = append(kv, htmlKV{"real CI labels", re.Status})
		if l.Available {
			kv = append(kv, htmlKV{"label build", fmt.Sprintf("%s, %s, cutoff %s", l.BuildID, l.Freshness, l.Cutoff)},
				htmlKV{"strong units", strconv.Itoa(l.StrongUnits)}, htmlKV{"raw failing cases", strconv.Itoa(l.RawFailingCases)})
		}
	}
	return kv
}

func orDash(s, prefix string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}
