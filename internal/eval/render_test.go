package eval

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/labels"
)

// sampleReport is a report of both populations: a synthetic section computed
// from the fixture events, and an empty real population with 14 raw failing
// cases.
func sampleReport(t *testing.T) (Report, Built) {
	t.Helper()
	tune, test := tuneTestFixture()
	test[0].Kind = KindCfg
	plan := basePlan(t)
	plan.Predictors = []PredictorState{{Kind: "static", ID: RowP0}, {Kind: "jev", ID: RowP2, Status: "needs OPENROUTER_API_KEY"}}
	built := BuildSynthetic(plan, tune, test)
	real := BuildReal(RealLabels{Available: true, BuildID: "b1", Cutoff: "2026-01-01", Freshness: labels.FreshnessCurrent, RawFailingCases: 14, Split: &UnitSplit{Method: SplitTemporal, At: "2026-06-01"}}, plan)
	return Report{
		Schema: SchemaVersion, Tool: "hazard eval",
		Inputs: Inputs{
			Source: "both", Split: SplitTemporal, SplitAt: "2026-06-01", Recall: 0.99, Bootstrap: 40, Seed: 1, Grid: "0.05:0.5:0.05",
			Baselines:  plan.Baselines,
			Predictors: []PredictorInfo{{ID: RowP0, Kind: "static", Status: StatusMeasured, WeightsDigest: "sha256:abc"}, {ID: RowP2, Kind: "jev", Status: "needs OPENROUTER_API_KEY"}},
			Durations:  DurationsInfo{Available: true, Path: "m.baseline.json", BaseSHA: "abc123", Known: 11, Imputed: 2},
		},
		Synthetic: &Synthetic{
			Population: SyntheticLabel, MutationBase: "abc123", PMode: "coverage",
			Observed: []Count{{Partition: "test", Status: "killed", N: 3}}, Skipped: map[string]int{"build_error": 1},
			Events:    EventCounts{Tune: 3, TuneWithFailures: 3, Test: 3, TestWithFailures: 3, TestByKind: map[string]int{KindCfg: 1, KindGo: 2}, TrainNotRead: 7},
			Rows:      built.Rows, Reliability: built.Reliability, PairsFile: FilePairs,
		},
		Real:  &real,
		Files: Files{Report: FileReport, Pairs: FilePairs, HTML: FileHTML, Reliability: FileReliability},
	}, built
}

func TestJSONCarriesSchemaPopulationsAndCounts(t *testing.T) {
	rep, _ := sampleReport(t)
	data, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("report.json is not JSON: %v", err)
	}
	if doc["schema_version"] != float64(SchemaVersion) || doc["tool"] != "hazard eval" {
		t.Errorf("header = %v %v", doc["schema_version"], doc["tool"])
	}
	syn := doc["synthetic"].(map[string]any)
	if syn["population"] != SyntheticLabel || syn["pairs_file"] != FilePairs {
		t.Errorf("synthetic = %v", syn)
	}
	for _, r := range syn["rows"].([]any) {
		if row := r.(map[string]any); row["population"] != SyntheticLabel {
			t.Errorf("row %v population = %v", row["id"], row["population"])
		}
	}
	real := doc["real"].(map[string]any)
	counts := real["labels"].(map[string]any)
	if counts["raw_failing_cases"] != float64(14) || counts["strong_units"] != float64(0) {
		t.Errorf("real labels = %v, want the exact counts 14 and 0", counts)
	}
	for _, r := range real["rows"].([]any) {
		row := r.(map[string]any)
		for name, m := range row["metrics"].(map[string]any) {
			if cell := m.(map[string]any); cell["value"] != nil || cell["na"] != NotAvailable {
				t.Errorf("real row %v %s = %v, want a null value and na=N/A", row["id"], name, cell)
			}
		}
	}
	if !strings.Contains(string(data), `"bins"`) {
		t.Error("report.json has no reliability bins")
	}
}

func TestTextCaptionsCarryTheSyntheticLabelAndTheRealCounts(t *testing.T) {
	rep, _ := sampleReport(t)
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Ablation, " + SyntheticLabel,
		"By change kind, " + SyntheticLabel,
		"Reliability of P0 (static), " + SyntheticLabel,
		"Real CI labels",
		"N/A (0 strong units, 14 raw failing cases)",
		"B0 run everything", "P2 + Jev", "needs OPENROUTER_API_KEY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	// The real table shows N/A with counts, never a number.
	realPart := between(out, "Real CI labels", "")
	for _, bad := range []string{"0.000", "$0.0000"} {
		if strings.Contains(realPart, bad) {
			t.Errorf("real section prints %q:\n%s", bad, realPart)
		}
	}
	for _, line := range strings.Split(out[:strings.Index(out, "Real CI labels")], "\n") {
		if strings.HasPrefix(line, "P0 ") && !strings.Contains(line, "[") {
			t.Errorf("P0 row has no interval: %q", line)
		}
	}
}

func TestHTMLIsStaticLabelledAndHasTheDiagram(t *testing.T) {
	rep, _ := sampleReport(t)
	data, err := rep.HTML()
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	title := between(page, "<title>", "</title>")
	if !strings.Contains(title, SyntheticLabel) {
		t.Errorf("title = %q, want the synthetic label", title)
	}
	for _, banned := range []string{"<script", "<link", "<img", "src=", "href=", "http://", "https://", "@import", "url("} {
		if strings.Contains(page, banned) {
			t.Errorf("page contains %q: it must be static and self-contained", banned)
		}
	}
	// Every table of the synthetic population is captioned with the label.
	captions := 0
	rest := page
	for {
		i := strings.Index(rest, "<caption>")
		if i < 0 {
			break
		}
		j := strings.Index(rest[i:], "</caption>")
		caption := rest[i : i+j]
		rest = rest[i+j:]
		captions++
		if strings.Contains(caption, "Real CI labels") {
			continue
		}
		if !strings.Contains(caption, SyntheticLabel) {
			t.Errorf("caption %q lacks the synthetic label", caption)
		}
	}
	if captions < 4 {
		t.Errorf("found %d captions, want the ablation, by-kind, reliability and real tables", captions)
	}
	for _, want := range []string{"<svg", "Data provenance", "N/A (0 strong units, 14 raw failing cases)", "needs OPENROUTER_API_KEY", "abc123"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestPairsCSVStartsWithTheLabelAndRoundTripsProbabilities(t *testing.T) {
	pairs := []Pair{
		{Config: RowP0, Event: "m1", Test: "go:p:TestA", P: 0.1 + 0.2, Label: true},
		{Config: RowP0, Event: "m1", Test: "go:p:Test,B", P: 1, Label: false},
	}
	var buf bytes.Buffer
	if err := WritePairs(&buf, pairs); err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(buf.String(), "\n"); first != "# "+SyntheticLabel {
		t.Errorf("first line = %q, want the synthetic label as a comment", first)
	}
	r := csv.NewReader(&buf)
	r.Comment = '#'
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || strings.Join(rows[0], ",") != "event,test,p,label,config" {
		t.Fatalf("rows = %v", rows)
	}
	if p, err := strconv.ParseFloat(rows[1][2], 64); err != nil || p != 0.1+0.2 || rows[1][3] != "1" || rows[2][3] != "0" || rows[2][1] != "go:p:Test,B" {
		t.Errorf("rows = %v: want the exact probability, labels 1 and 0, and the comma kept in a quoted field", rows[1:])
	}
}

func TestReliabilityCSVLeavesEmptyBinsEmpty(t *testing.T) {
	_, built := sampleReport(t)
	var buf bytes.Buffer
	if err := WriteReliability(&buf, built.Reliability); err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(buf.String(), "\n"); first != "# "+SyntheticLabel {
		t.Errorf("first line = %q, want the synthetic label as a comment", first)
	}
	rows := readCSV(t, &buf)
	if len(rows) != 11 || strings.Join(rows[0], ",") != "config,bin,lo,hi,count,mean_p,observed_rate" {
		t.Fatalf("rows = %v", rows)
	}
	if bin3 := rows[4]; bin3[0] != "P0" || bin3[1] != "3" || bin3[4] != "0" || bin3[5] != "" || bin3[6] != "" {
		t.Errorf("empty bin 3 = %v, want count 0 and no mean or rate (never 0)", bin3)
	}
	if bin1 := rows[2]; bin1[4] != "3" || bin1[6] != "1" {
		t.Errorf("bin 1 = %v, want 3 pairs observed at rate 1", bin1)
	}
}

func readCSV(t *testing.T, r io.Reader) [][]string {
	t.Helper()
	cr := csv.NewReader(r)
	cr.Comment = '#'
	rows, err := cr.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Guard 3 through the renderers: an empty real population never prints a zero.
func TestEmptyRealPopulationPrintsNoZeroInAnyRendering(t *testing.T) {
	rep, _ := sampleReport(t)
	rep.Synthetic = nil
	rep.Inputs.Source = "ci"
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	page, err := rep.HTML()
	if err != nil {
		t.Fatal(err)
	}
	data, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"text": buf.String(), "html": string(page)} {
		if !strings.Contains(out, "N/A (0 strong units, 14 raw failing cases)") {
			t.Errorf("%s lacks the N/A row with its counts:\n%s", name, out)
		}
		for _, bad := range []string{"0.000", "$0.0000", "<td>0</td>"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s prints %q for an empty population", name, bad)
			}
		}
		if strings.Contains(out, SyntheticLabel) {
			t.Errorf("%s names the synthetic population although none was measured", name)
		}
	}
	if strings.Contains(string(data), `"value": 0`) {
		t.Errorf("report.json has a zero value for an empty population")
	}
}

// between is the text from the first start to the end of the string or, when end
// is not empty, to the first end after it; empty when start is absent.
func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i:]
	if j := strings.Index(s, end); end != "" && j >= 0 {
		s = s[:j+len(end)]
	}
	return s
}
