package eval

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// pairsPython returns the interpreter for the cross-check: HAZARD_TEST_PYTHON,
// else python on PATH. When pytest cannot be imported through it the test is
// skipped, unless HAZARD_REQUIRE_PYTEST is "1", in which case it fails.
func pairsPython(t *testing.T) string {
	t.Helper()
	python := os.Getenv("HAZARD_TEST_PYTHON")
	if python == "" {
		python = "python"
	}
	if out, err := exec.Command(python, "-c", "import pytest").CombinedOutput(); err != nil {
		msg := fmt.Sprintf("pytest is unavailable through %s: %s %v", python, bytes.TrimSpace(out), err)
		if os.Getenv("HAZARD_REQUIRE_PYTEST") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg + " (set HAZARD_TEST_PYTHON to an interpreter with pytest, HAZARD_REQUIRE_PYTEST=1 to forbid skipping)")
	}
	return python
}

// crossPairs are the scored pairs of two configurations: bin edges and values
// that float rounding places on either side of them, ties, and for P1 no
// probability under 0.55, so that its low bins are empty.
func crossPairs() []Pair {
	rng := rand.New(rand.NewPCG(11, 0))
	edges := []float64{0, 1e-12, 0.05, 0.1, 0.2, 0.3, 0.5, 0.7, 0.9, 0.95, 0.999999999, 1}
	var pairs []Pair
	add := func(config string, i int, p float64) {
		pairs = append(pairs, Pair{
			Config: config, Event: fmt.Sprintf("m%03d", i/7), Test: fmt.Sprintf("go:example.com/m/p%d:Test%d", i%5, i), P: p, Label: rng.Float64() < p,
		})
	}
	for i := 0; i < 600; i++ {
		p := rng.Float64()
		if i%4 == 0 {
			p = math.Round(p*100) / 100 // many ties on the second decimal
		}
		add("P0", i, p)
		add("P1", i, 0.55+0.45*rng.Float64())
	}
	for i, p := range edges {
		add("P0", 1000+i, p)
		add("P1", 1000+i, max(p, 0.55))
	}
	return pairs
}

type pyBin struct {
	Bin      int      `json:"bin"`
	Lo       float64  `json:"lo"`
	Hi       float64  `json:"hi"`
	Count    int      `json:"count"`
	MeanP    *float64 `json:"mean_p"`
	Observed *float64 `json:"observed_rate"`
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) <= 1e-12
}

func compareBins(t *testing.T, what, config string, got []pyBin, want []Bin) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s %s: %d bins, want %d", what, config, len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Bin != w.Index || g.Count != w.Count || math.Abs(g.Lo-w.Lo) > 1e-12 || math.Abs(g.Hi-w.Hi) > 1e-12 ||
			!sameFloat(g.MeanP, w.MeanP) || !sameFloat(g.Observed, w.Observed) {
			t.Errorf("%s %s bin %d: %+v mean=%v obs=%v, want %+v mean=%v obs=%v", what, config, i, g, ptr(g.MeanP), ptr(g.Observed), w, ptr(w.MeanP), ptr(w.Observed))
		}
	}
}

// TestReliabilityAgreesWithThePythonReference writes the pairs.csv of a
// fixture with WritePairs, lets research/hazard_research/reliability.py
// recompute the 10-bin table from the file, and requires the bins of Go and of
// the reliability.csv it writes to agree with it to 1e-12 (counts exactly).
func TestReliabilityAgreesWithThePythonReference(t *testing.T) {
	python := pairsPython(t)
	researchDir, err := filepath.Abs(filepath.Join("..", "..", "research"))
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(researchDir, "hazard_research", "reliability.py")); err != nil || st.IsDir() {
		t.Fatalf("reference implementation missing under %s: %v", researchDir, err)
	}

	pairs := crossPairs()
	var buf bytes.Buffer
	if err := WritePairs(&buf, pairs); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "pairs.csv")
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-m", "hazard_research.reliability", "--pairs", file)
	cmd.Dir = researchDir
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "PYTHONIOENCODING=utf-8")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python reference failed: %v\n%s", err, stderr.String())
	}
	var res struct {
		Configs map[string][]pyBin `json:"configs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("python output is not the expected JSON: %v\n%.300s", err, stdout.String())
	}

	byConfig := map[string][]ProbLabel{}
	var rels []Reliability
	for _, p := range pairs {
		byConfig[p.Config] = append(byConfig[p.Config], ProbLabel{Test: p.Test, P: p.P, Label: p.Label})
	}
	for _, config := range []string{"P0", "P1"} {
		bins := ReliabilityBins(byConfig[config])
		rels = append(rels, Reliability{Config: config, Population: SyntheticLabel, Pairs: len(byConfig[config]), Bins: bins})
		compareBins(t, "go", config, res.Configs[config], bins)
	}
	if len(res.Configs) != 2 {
		t.Errorf("python tabulated %d configurations, want 2", len(res.Configs))
	}
	if res.Configs["P1"][2].Count != 0 || res.Configs["P1"][2].MeanP != nil {
		t.Errorf("P1 bin 2 = %+v, want empty without a mean (the fixture has no P1 probability under 0.55)", res.Configs["P1"][2])
	}

	// The reliability.csv of the same bins reads back to the same table.
	var relCSV bytes.Buffer
	if err := WriteReliability(&relCSV, rels); err != nil {
		t.Fatal(err)
	}
	r := csv.NewReader(&relCSV)
	r.Comment = '#'
	rows, err := r.ReadAll()
	if err != nil || len(rows) != 1+2*bins {
		t.Fatalf("reliability.csv: %d rows, %v", len(rows), err)
	}
	fromCSV := map[string][]pyBin{}
	for _, row := range rows[1:] {
		var b pyBin
		b.Bin, _ = strconv.Atoi(row[1])
		b.Lo, _ = strconv.ParseFloat(row[2], 64)
		b.Hi, _ = strconv.ParseFloat(row[3], 64)
		b.Count, _ = strconv.Atoi(row[4])
		if row[5] != "" {
			v, _ := strconv.ParseFloat(row[5], 64)
			b.MeanP = &v
		}
		if row[6] != "" {
			v, _ := strconv.ParseFloat(row[6], 64)
			b.Observed = &v
		}
		fromCSV[row[0]] = append(fromCSV[row[0]], b)
	}
	for config, want := range res.Configs {
		got := fromCSV[config]
		for i, w := range want {
			g := got[i]
			if g.Count != w.Count || !sameFloat(g.MeanP, w.MeanP) || !sameFloat(g.Observed, w.Observed) {
				t.Errorf("reliability.csv %s bin %d = %+v, python says %+v", config, i, g, w)
			}
		}
	}
}
