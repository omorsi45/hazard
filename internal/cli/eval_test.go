package cli

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/eval"
	"github.com/omorsi45/hazard/internal/index"
)

// evalDoc is the part of report.json these tests read.
type evalDoc struct {
	Schema int `json:"schema_version"`
	Inputs struct {
		Predictors []struct{ ID, Kind, Status string } `json:"predictors"`
		Durations  struct {
			Available bool `json:"available"`
			Known     int  `json:"known_tests"`
			Imputed   int  `json:"imputed_tests"`
		} `json:"durations"`
	} `json:"inputs"`
	Synthetic *struct {
		Population   string `json:"population"`
		MutationBase string `json:"mutation_base"`
		PMode        string `json:"p_mode"`
		Events       struct {
			Tune         int `json:"tune"`
			Test         int `json:"test"`
			TrainNotRead int `json:"train_not_read"`
		} `json:"events"`
		Rows        []evalRow `json:"rows"`
		Reliability []struct {
			Config string `json:"config"`
			Pairs  int    `json:"pairs"`
		} `json:"reliability"`
	} `json:"synthetic"`
	Real *struct {
		Population string    `json:"population"`
		Status     string    `json:"status"`
		Rows       []evalRow `json:"rows"`
	} `json:"real"`
	Files map[string]string `json:"files"`
}

type evalRow struct {
	ID         string `json:"id"`
	Population string `json:"population"`
	Status     string `json:"status"`
	Note       string `json:"note"`
	Flag       string `json:"recall_flag"`
	Metrics    map[string]struct {
		Value *float64 `json:"value"`
		NA    string   `json:"na"`
	} `json:"metrics"`
}

func (d evalDoc) row(t *testing.T, id string) evalRow {
	t.Helper()
	if d.Synthetic != nil {
		for _, r := range d.Synthetic.Rows {
			if r.ID == id {
				return r
			}
		}
	}
	t.Fatalf("no synthetic row %s", id)
	return evalRow{}
}

func (f *trainFixture) runEval(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Chdir(f.dir)
	return invoke(context.Background(), append([]string{"eval"}, args...)...)
}

func (f *trainFixture) readReport(t *testing.T, dir string) evalDoc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d evalDoc
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("report.json: %v", err)
	}
	return d
}

func (f *trainFixture) writeDurations(t *testing.T, rel, content string) {
	t.Helper()
	full := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEvalEndToEnd(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	f := newTrainFixture(t)
	f.seed(t, f.standardRows(t))
	if code, out, errOut := f.runTrain(t, "--before", "2026-01-02", "--json"); code != 0 {
		t.Fatalf("train: code=%d err=%q out=%s", code, errOut, out)
	}
	f.writeDurations(t, ".hazard/mutants.jsonl.baseline.json", `{"base_sha":"`+f.base+`","tests":{"`+trainTestA+`":10,"`+trainTestB+`":30}}`)
	reportDir := filepath.Join(f.dir, ".hazard", "report")

	t.Run("writes the four files and labels the mutation population synthetic", func(t *testing.T) {
		code, out, errOut := f.runEval(t, "--bootstrap", "50")
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q out=%s", code, errOut, out)
		}
		for _, name := range []string{"report.json", "pairs.csv", "report.html", "reliability.csv"} {
			if _, err := os.Stat(filepath.Join(reportDir, name)); err != nil {
				t.Errorf("%s was not written: %v", name, err)
			}
		}
		for _, want := range []string{"Ablation, " + eval.SyntheticLabel, "B0 run everything", "B1 L1 graph only", "P0 + static rank", "needs OPENROUTER_API_KEY", "Real CI labels: N/A"} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
		}
		pairs, err := os.ReadFile(filepath.Join(reportDir, "pairs.csv"))
		if err != nil || !strings.HasPrefix(string(pairs), "# "+eval.SyntheticLabel+"\nevent,test,p,label,config\n") {
			t.Errorf("pairs.csv = %.100q (%v)", pairs, err)
		}
		page, err := os.ReadFile(filepath.Join(reportDir, "report.html"))
		if err != nil || !strings.Contains(string(page), "<title>Hazard eval: "+eval.SyntheticLabel) {
			t.Errorf("report.html title: %v", err)
		}

		d := f.readReport(t, reportDir)
		if d.Schema != eval.SchemaVersion || d.Synthetic == nil || d.Synthetic.Population != eval.SyntheticLabel {
			t.Fatalf("report = %+v", d)
		}
		if d.Synthetic.MutationBase != f.base || d.Synthetic.PMode != "graph" {
			t.Errorf("base=%q mode=%q, want %s and the graph layer (the fixture index has no coverage)", d.Synthetic.MutationBase, d.Synthetic.PMode, f.base)
		}
		// The fixture holds 12 train-partition mutants and 2 each in tune and test.
		if ev := d.Synthetic.Events; ev.Tune != 2 || ev.Test != 2 || ev.TrainNotRead != 12 {
			t.Errorf("events = %+v, want tune 2, test 2 and 12 train mutants counted and not read", ev)
		}
		if !d.Inputs.Durations.Available || d.Inputs.Durations.Known != 2 {
			t.Errorf("durations = %+v, want the 2 measured tests of the default baseline file", d.Inputs.Durations)
		}
		for _, r := range d.Synthetic.Rows {
			if r.Population != eval.SyntheticLabel {
				t.Errorf("row %s population = %q", r.ID, r.Population)
			}
		}
		var ids []string
		for _, r := range d.Synthetic.Rows {
			ids = append(ids, r.ID)
		}
		if got := strings.Join(ids, ","); got != "B0,B1,B2,P0,P1,P2,P3" {
			t.Errorf("rows = %s", got)
		}

		b0, b1, b2 := d.row(t, "B0"), d.row(t, "B1"), d.row(t, "B2")
		if v := b0.Metrics["effective_saving"].Value; v == nil || *v != 0 {
			t.Errorf("B0 effective saving = %v, want 0", v)
		}
		if v := b0.Metrics["recall_commit"].Value; v == nil || *v != 1 {
			t.Errorf("B0 recall = %v, want 1", v)
		}
		// L1 selects TestA only (max_hops 0): it costs 10 of 40 ms, and the killed mutant
		// is also caught by TestB, which L1 does not select.
		if v := b1.Metrics["time_ratio"].Value; v == nil || math.Abs(*v-0.25) > 1e-9 {
			t.Errorf("B1 time ratio = %v, want 0.25", v)
		}
		if v := b1.Metrics["recall_commit"].Value; v == nil || *v != 0 || b1.Flag != "below 0.99" {
			t.Errorf("B1 recall = %v flag=%q, want 0 flagged below 0.99", v, b1.Flag)
		}
		if b2.Status != "unavailable: the index has no coverage" || len(b2.Metrics) != 0 {
			t.Errorf("B2 = %+v, want unavailable without coverage", b2)
		}
		for _, id := range []string{"P2", "P3"} {
			if r := d.row(t, id); r.Status != "needs OPENROUTER_API_KEY" || len(r.Metrics) != 0 {
				t.Errorf("%s = %+v, want the key requirement and no numbers", id, r)
			}
		}
		for _, id := range []string{"P0", "P1"} {
			r := d.row(t, id)
			if r.Status != "measured" || r.Metrics["recall_commit"].Value == nil {
				t.Errorf("%s = %+v, want measured", id, r)
			}
		}

		// The label build of the fixture is absent: the real population is N/A with its reason.
		if d.Real == nil || d.Real.Population != eval.RealLabel || !strings.HasPrefix(d.Real.Status, "N/A") {
			t.Fatalf("real = %+v, want N/A", d.Real)
		}
		for _, r := range d.Real.Rows {
			for name, m := range r.Metrics {
				if m.Value != nil || m.NA != "N/A" {
					t.Errorf("real %s %s = %+v, want N/A", r.ID, name, m)
				}
			}
		}
	})

	t.Run("json prints the report on stdout", func(t *testing.T) {
		code, out, errOut := f.runEval(t, "--bootstrap", "20", "--json", "--out", "other")
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		var d evalDoc
		if err := json.Unmarshal([]byte(out), &d); err != nil || d.Synthetic == nil || d.Synthetic.Population != eval.SyntheticLabel {
			t.Fatalf("stdout is not the report: %v\n%.300s", err, out)
		}
		if _, err := os.Stat(filepath.Join(f.dir, "other", "report.json")); err != nil {
			t.Errorf("--out was not honored: %v", err)
		}
	})

	t.Run("source ci writes no mutation numbers", func(t *testing.T) {
		code, out, errOut := f.runEval(t, "--source", "ci", "--out", "ci-only")
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q out=%s", code, errOut, out)
		}
		if strings.Contains(out, eval.SyntheticLabel) {
			t.Errorf("stdout names the mutation population although none was measured:\n%s", out)
		}
		d := f.readReport(t, filepath.Join(f.dir, "ci-only"))
		if d.Synthetic != nil || d.Real == nil {
			t.Errorf("report = %+v, want only the real section", d)
		}
		for _, name := range []string{"pairs.csv", "reliability.csv"} {
			if _, err := os.Stat(filepath.Join(f.dir, "ci-only", name)); err == nil {
				t.Errorf("%s was written for a run without the mutation population", name)
			}
		}
	})

	t.Run("a missing explicit durations file is a usage error", func(t *testing.T) {
		code, _, errOut := f.runEval(t, "--durations", "nowhere.json")
		if code != 2 || !strings.Contains(errOut, "nowhere.json") {
			t.Errorf("code=%d err=%q, want 2 naming the file", code, errOut)
		}
	})

	t.Run("without durations the time cells are N/A", func(t *testing.T) {
		if err := os.Remove(filepath.Join(f.dir, ".hazard", "mutants.jsonl.baseline.json")); err != nil {
			t.Fatal(err)
		}
		defer f.writeDurations(t, ".hazard/mutants.jsonl.baseline.json", `{"base_sha":"`+f.base+`","tests":{"`+trainTestA+`":10,"`+trainTestB+`":30}}`)
		code, out, errOut := f.runEval(t, "--bootstrap", "20", "--out", "nodur", "--json")
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		var d evalDoc
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatal(err)
		}
		if d.Inputs.Durations.Available {
			t.Errorf("durations = %+v, want unavailable", d.Inputs.Durations)
		}
		for _, id := range []string{"B0", "B1", "P0"} {
			r := d.row(t, id)
			for _, name := range []string{"time_ratio", "effective_saving"} {
				if m := r.Metrics[name]; m.Value != nil || !strings.HasPrefix(m.NA, "N/A") {
					t.Errorf("%s %s = %+v, want N/A", id, name, m)
				}
			}
			if r.Metrics["recall_commit"].Value == nil {
				t.Errorf("%s lost its recall", id)
			}
		}
	})

	t.Run("a predictor without its artifact reads unavailable, not zero", func(t *testing.T) {
		removeArtifacts(t, f.dir)
		code, out, errOut := f.runEval(t, "--bootstrap", "20", "--out", "noart", "--json")
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		var d evalDoc
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"P0", "P1"} {
			r := d.row(t, id)
			if !strings.HasPrefix(r.Status, "unavailable: ") || len(r.Metrics) != 0 {
				t.Errorf("%s = %+v, want unavailable with the reason and no numbers", id, r)
			}
		}
		if len(d.Synthetic.Reliability) != 0 {
			t.Errorf("reliability tables = %+v for predictors that could not be scored", d.Synthetic.Reliability)
		}
		if r := d.row(t, "B1"); r.Metrics["recall_commit"].Value == nil {
			t.Error("B1 needs no artifact and must still be measured")
		}
	})

	t.Run("refuses when the index does not describe the mutation base", func(t *testing.T) {
		rows := f.standardRows(t)
		for i := range rows {
			rows[i].base = f.ciParent
		}
		f.seed(t, rows)
		code, out, errOut := f.runEval(t, "--out", "refused")
		if code != 1 || !strings.Contains(out, "eval: refused: the index describes "+f.base) || !strings.Contains(errOut, "refused") {
			t.Errorf("code=%d out=%q err=%q, want exit 1 and the refusal", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(f.dir, "refused")); err == nil {
			t.Error("a refused run wrote a report")
		}
	})
}

func TestEvalWithoutADatabaseRefuses(t *testing.T) {
	f := newTrainFixture(t)
	code, out, errOut := f.runEval(t)
	if code != 1 || !strings.Contains(out, "eval: refused: there is no database") || !strings.Contains(errOut, "refused") {
		t.Errorf("code=%d out=%q err=%q, want exit 1 and the refusal", code, out, errOut)
	}
}

func TestEvalUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"eval", "--source", "everything"},
		{"eval", "--recall", "1.5"},
		{"eval", "--recall", "0"},
		{"eval", "--bootstrap", "0"},
		{"eval", "--predictors", "static,oracle"},
		{"eval", "--baselines", "all,l2"},
		{"eval", "--split", "sideways"},
		{"eval", "--split", "random", "--split-at", "2026-01-01"},
		{"eval", "--split-at", "June"},
		{"eval", "--out", ""},
		{"eval", "extra"},
	} {
		code, _, errOut := invoke(context.Background(), args...)
		if code != 2 || errOut == "" {
			t.Errorf("%v: code=%d err=%q, want a usage error (2)", args, code, errOut)
		}
	}
}

func TestEvalHelpNamesTheDefaults(t *testing.T) {
	code, out, errOut := invoke(context.Background(), "eval", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, want := range []string{"--source", "--recall", "--bootstrap", "--seed", "--durations", "--predictors", "--baselines", "--split-at", "synthetic (mutation labels)", "N/A"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

// The documented flags of seed 6.7 parse. The context is canceled so that the
// command stops after parsing and reads nothing of the surrounding repository.
func TestEvalDocumentedFlagsParse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{
		{"eval", "--split", "temporal", "--split-at", "2026-06-01", "--predictors", "static,logistic,jev,ensemble", "--baselines", "all,l1-only,l1-coverage", "--out", "report"},
		{"eval", "--split", "random", "--source", "mutation", "--recall", "0.95", "--bootstrap", "100", "--seed", "7", "--durations", "m.baseline.json", "--config", "hazard.yml", "--json"},
	} {
		code, _, errOut := invoke(ctx, args...)
		if code != 70 || !strings.Contains(errOut, "canceled") {
			t.Errorf("%v: code=%d err=%q, want the flags to parse and the canceled context to stop the command", args, code, errOut)
		}
	}
}

// make eval runs hazard eval with its defaults.
func TestMakefileHasAnEvalTarget(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	var phony, recipe bool
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, ".PHONY:") && slices.Contains(strings.Fields(line), "eval") {
			phony = true
		}
		if line == "eval:" && i+1 < len(lines) {
			recipe = lines[i+1] == "\tgo run ./cmd/hazard eval"
		}
	}
	if !phony || !recipe {
		t.Errorf("Makefile: eval is phony=%v, runs hazard eval with its defaults=%v", phony, recipe)
	}
}

func parseEvalDoc(t *testing.T, out string) evalDoc {
	t.Helper()
	var d evalDoc
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("stdout is not the report: %v\n%.300s", err, out)
	}
	return d
}

// With coverage in the index, B2 is measured and the predictors score the
// coverage layer.
func TestEvalWithACoverageIndex(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	f := newTrainFixture(t)
	f.seed(t, f.standardRows(t))
	if code, out, errOut := f.runTrain(t, "--before", "2026-01-02", "--json"); code != 0 {
		t.Fatalf("train: code=%d err=%q out=%s", code, errOut, out)
	}
	cfg, err := config.Load(filepath.Join(f.dir, "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := index.Build(context.Background(), f.dir, cfg, index.Options{Coverage: true})
	if err != nil {
		t.Fatalf("index.Build with coverage: %v", err)
	}
	if m.Coverage == nil || m.HeadSHA != f.base {
		t.Fatalf("index = head %s coverage %v, want coverage at %s", m.HeadSHA, m.Coverage != nil, f.base)
	}
	if err := index.Publish(f.dir, m); err != nil {
		t.Fatal(err)
	}
	f.writeDurations(t, ".hazard/mutants.jsonl.baseline.json", `{"base_sha":"`+f.base+`","tests":{"`+trainTestA+`":10,"`+trainTestB+`":30}}`)

	code, out, errOut := f.runEval(t, "--bootstrap", "30", "--json", "--source", "mutation")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q out=%s", code, errOut, out)
	}
	d := parseEvalDoc(t, out)
	if d.Synthetic == nil || d.Synthetic.PMode != "coverage" {
		t.Fatalf("report = %+v, want the predictors to score the coverage layer", d.Synthetic)
	}
	if b2 := d.row(t, "B2"); b2.Status != "measured" || b2.Metrics["recall_commit"].Value == nil {
		t.Errorf("B2 = %+v, want measured", b2)
	}
	for _, id := range []string{"P0", "P1"} {
		if r := d.row(t, id); r.Status != "measured" {
			t.Errorf("%s = %+v, want measured on the coverage layer", id, r)
		}
	}
	if d.Real != nil {
		t.Errorf("a mutation-only run has a real section: %+v", d.Real)
	}
}
