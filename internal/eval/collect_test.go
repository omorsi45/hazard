package eval

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/events"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/mutate"
	"github.com/omorsi45/hazard/internal/store"
)

func TestKindOfSplitsGoFromConfiguration(t *testing.T) {
	for file, want := range map[string]string{
		"calc/calc.go": KindGo, "main.GO": KindGo, "conf/settings.yml": KindCfg, "Makefile": KindCfg, "a/b.go.tmpl": KindCfg,
	} {
		if got := kindOf(file); got != want {
			t.Errorf("kindOf(%q) = %q, want %q", file, got, want)
		}
	}
}

func decisionFixture() engine.Decision {
	return engine.Decision{
		OverheadMS: 7,
		L1: &impact.Result{
			Certain:       []impact.Member{{TestID: "go:p:TestAlways", Reason: "always_run"}, {TestID: "go:p:TestCert", Reason: "direct"}},
			Candidates:    []impact.Candidate{{TestID: "go:p:TestRes1"}, {TestID: "go:p:TestRes2"}},
			RawCandidates: 4, Truncated: 2, PackageCount: 3, ChangedPackages: []string{"p"},
			AllTests: []string{"go:p:TestAlways", "go:p:TestCert", "go:p:TestCut1", "go:p:TestCut2", "go:p:TestRes1", "go:p:TestRes2"},
		},
		Change: &change.ChangeSet{},
	}
}

func TestLayerKeepsCertainAndSeparatesAlwaysRunForTheController(t *testing.T) {
	b := layerBuilder{cfg: config.Defaults(), testMS: map[string]int{"go:p:TestCert": 4}, testPackage: map[string]string{"go:p:TestCert": "p"}}
	failing := map[string]bool{"go:p:TestCut1": true, "go:p:TestRes1": true, "go:p:TestUnknown": true}
	l, all := b.layer(decisionFixture(), failing)

	if l.OverheadMS != 7 || l.Stopped != "" || l.FullReason != "" {
		t.Errorf("layer = %+v, want overhead 7 and no stop", l)
	}
	if want := []string{"go:p:TestAlways", "go:p:TestCert"}; !reflect.DeepEqual(l.Certain, want) {
		t.Errorf("Certain = %v, want %v (the always-run test included: it is part of B1 and B2)", l.Certain, want)
	}
	// TestCut1 is in the inventory, neither CERTAIN nor listed: the cap cut it.
	// TestRes1 is listed, TestUnknown is not in the inventory at all.
	if l.TruncMissed != 1 {
		t.Errorf("TruncMissed = %d, want 1", l.TruncMissed)
	}
	in := l.Input
	if !reflect.DeepEqual(in.Certain, []string{"go:p:TestCert"}) || !reflect.DeepEqual(in.AlwaysRun, []string{"go:p:TestAlways"}) {
		t.Errorf("controller input certain=%v always=%v, want the always-run test apart", in.Certain, in.AlwaysRun)
	}
	if in.RawCandidates != 4 || in.Truncated != 2 || in.PackageCount != 3 || in.ChangedPackages != 1 || in.TestMS["go:p:TestCert"] != 4 || in.TestPackage["go:p:TestCert"] != "p" {
		t.Errorf("controller input = %+v", in)
	}
	if !reflect.DeepEqual(in.AllTests, all) || len(all) != 6 {
		t.Errorf("inventory = %v and %v, want the 6 tests of L1", in.AllTests, all)
	}
	if in.Lambda != 0 || in.Candidates != nil {
		t.Errorf("lambda and candidates are the evaluator's to set, got %v %v", in.Lambda, in.Candidates)
	}
}

func TestLayerCarriesFullSuiteDemands(t *testing.T) {
	d := decisionFixture()
	d.L1.FullReason = "unmapped_change:x.go"
	d.Change.TriggersFull = []string{"go.mod"}
	l, _ := layerBuilder{cfg: config.Defaults()}.layer(d, nil)
	if l.FullReason != "unmapped_change:x.go" || l.Input.FullReason != l.FullReason || !reflect.DeepEqual(l.Input.TriggersFull, []string{"go.mod"}) || !l.demandsFull() {
		t.Errorf("layer = %+v, want the full-suite reason and trigger on the controller input", l)
	}
}

func TestLayerOfAnEngineThatStoppedBeforeL1IsStopped(t *testing.T) {
	d := engine.Decision{OverheadMS: 3, AbstainReason: "index_stale", AllTests: []string{"go:p:TestA"}}
	l, all := layerBuilder{cfg: config.Defaults()}.layer(d, nil)
	if l.Stopped != "index_stale" || !l.demandsFull() || l.OverheadMS != 3 || !reflect.DeepEqual(all, []string{"go:p:TestA"}) {
		t.Errorf("layer = %+v inventory=%v, want stopped on index_stale", l, all)
	}
	l, _ = layerBuilder{cfg: config.Defaults()}.layer(engine.Decision{}, nil)
	if l.Stopped == "" {
		t.Error("a decision with neither L1 nor a reason must still stop the layer: unknown is the full suite")
	}
}

func seedMutants(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hazard.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO mutants(id,file,line,operator,original,mutated,base_sha,status,created_at) VALUES('m2','calc/calc.go',3,'cmp','>','>=','base1','survived','2026-01-01T00:00:00Z')`,
		`INSERT INTO mutants(id,file,line,operator,original,mutated,base_sha,status,created_at) VALUES('m1','calc/calc.go',3,'cmp','>','>=','base1','killed','2026-01-01T00:00:00Z')`,
		`INSERT INTO mutants(id,file,line,operator,original,mutated,base_sha,status,created_at) VALUES('m3','calc/calc.go',3,'cmp','>','>=','base1','build_error','2026-01-01T00:00:00Z')`,
		`INSERT INTO mutant_kills(mutant_id,test_id) VALUES('m1','go:p:TestB'),('m1','go:p:TestA'),('m2','go:p:TestStray')`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadMutationRecordsAttachesKillsToKilledMutantsOnly(t *testing.T) {
	got, err := readMutationRecords(context.Background(), seedMutants(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []mutate.Record{
		{ID: "m1", File: "calc/calc.go", Line: 3, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "base1", Status: "killed", Killers: []string{"go:p:TestA", "go:p:TestB"}},
		{ID: "m2", File: "calc/calc.go", Line: 3, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "base1", Status: "survived"},
		{ID: "m3", File: "calc/calc.go", Line: 3, Operator: "cmp", Original: ">", Mutated: ">=", BaseSHA: "base1", Status: "build_error"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %+v\nwant      %+v", got, want)
	}
}

func TestReadMutationRecordsWithoutADatabaseIsARefusalAndCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "hazard.db")
	_, err := readMutationRecords(context.Background(), path)
	var refusal *events.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(filepath.Dir(path)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refusal left %s behind (stat: %v): a database must not be created to be read", filepath.Dir(path), statErr)
	}
}
