package events

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/mutate"
)

const (
	calcSrc = "package calc\n\nfunc Big(x int) bool { return x > 3 && x < 100 }\n"
	confSrc = "retries: 3\nname: demo\n"
)

func mutationFiles() map[string]string {
	return map[string]string{
		"go.mod":            "module example.com/m\n\ngo 1.21\n",
		"calc/calc.go":      calcSrc,
		"conf/settings.yml": confSrc,
	}
}

// recordOf builds the record the driver would store for mutant m.
func recordOf(m mutate.Mutant, base, status string, killers ...string) mutate.Record {
	return mutate.Record{
		ID: m.ID, File: m.File, Line: m.Line, Operator: m.Operator, Original: m.Original, Mutated: m.Mutated,
		BaseSHA: base, Status: status, Killers: killers,
	}
}

func cmpMutants(t *testing.T) []mutate.Mutant {
	t.Helper()
	ms, err := mutate.GoMutants("calc/calc.go", []byte(calcSrc), []string{"cmp"})
	if err != nil || len(ms) < 2 {
		t.Fatalf("GoMutants = %v, %v, want at least two cmp mutants", ms, err)
	}
	return ms
}

func TestMutationEventsKeepsKilledAndSurvivedAndCountsTheRest(t *testing.T) {
	dir, base := newRepo(t, mutationFiles())
	ms := cmpMutants(t)
	cfg := mutate.ConfigMutants("conf/settings.yml", []byte(confSrc))
	if len(cfg) == 0 {
		t.Fatal("ConfigMutants found nothing in the fixture")
	}
	recs := []mutate.Record{
		recordOf(ms[1], base, "survived"),
		recordOf(ms[0], base, "killed", "go:example.com/m/calc:TestB", "go:example.com/m/calc:TestA", "go:example.com/m/calc:TestB"),
		recordOf(cfg[0], base, "killed", "go:example.com/m/conf:TestConf"),
		{ID: "0000000000000001", File: "calc/calc.go", Operator: "cmp", BaseSHA: base, Status: "build_error"},
		{ID: "0000000000000002", File: "calc/calc.go", Operator: "cmp", BaseSHA: base, Status: "build_error"},
		{ID: "0000000000000003", File: "calc/calc.go", Operator: "cmp", BaseSHA: base, Status: "timeout"},
		{ID: "0000000000000004", File: "calc/calc.go", Operator: "cmp", BaseSHA: base, Status: "unconfirmed"},
	}
	events, contents, skipped, err := MutationEvents(context.Background(), dir, recs)
	if err != nil {
		t.Fatalf("MutationEvents: %v", err)
	}
	if want := map[string]int{"build_error": 2, "timeout": 1, "unconfirmed": 1}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
	if len(events) != 3 || len(contents) != 3 {
		t.Fatalf("got %d events and %d contents, want 3 and 3: %+v", len(events), len(contents), events)
	}
	ids := make([]string, len(events))
	for i, ev := range events {
		ids[i] = ev.ID
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("events are not ordered by ID: %v", ids)
	}
	byID := map[string]Event{}
	for _, ev := range events {
		byID[ev.ID] = ev
	}

	killed := byID[ms[0].ID]
	wantKilled := Event{
		ID: ms[0].ID, Origin: OriginMutation, Group: "mut:calc/calc.go", BaseSHA: base,
		Failing: []string{"go:example.com/m/calc:TestA", "go:example.com/m/calc:TestB"},
		File:    "calc/calc.go", Operator: "cmp", Line: ms[0].Line, Time: baseTime,
	}
	if !reflect.DeepEqual(killed, wantKilled) {
		t.Errorf("killed event = %+v\nwant         %+v", killed, wantKilled)
	}
	survived := byID[ms[1].ID]
	if len(survived.Failing) != 0 || survived.Group != "mut:calc/calc.go" || survived.Origin != OriginMutation {
		t.Errorf("survived event = %+v, want no failing tests in group mut:calc/calc.go", survived)
	}
	conf := byID[cfg[0].ID]
	if conf.Group != "mut:conf/settings.yml" || conf.Operator != "cfg" || !reflect.DeepEqual(conf.Failing, []string{"go:example.com/m/conf:TestConf"}) {
		t.Errorf("config event = %+v, want group mut:conf/settings.yml, operator cfg and its killer", conf)
	}
	for _, ev := range events {
		if ev.HeadSHA != "" {
			t.Errorf("event %s has HeadSHA %q before any replay", ev.ID, ev.HeadSHA)
		}
	}

	for _, tc := range []struct {
		m    mutate.Mutant
		src  string
		name string
	}{{ms[0], calcSrc, "killed"}, {ms[1], calcSrc, "survived"}, {cfg[0], confSrc, "config"}} {
		want, err := mutate.Apply([]byte(tc.src), tc.m)
		if err != nil {
			t.Fatal(err)
		}
		if got := contents[tc.m.ID]; string(got) != string(want) || string(got) == tc.src {
			t.Errorf("%s: mutated content = %q, want %q", tc.name, got, want)
		}
	}
}

func TestMutationEventsTimeIsTheCommitterTimeOfEachBase(t *testing.T) {
	dir, first := newRepo(t, mutationFiles())
	later := baseTime.Add(48 * time.Hour)
	writeFiles(t, dir, map[string]string{"calc/other.go": "package calc\n"})
	second := commitAt(t, dir, "second", later)
	ms := cmpMutants(t)
	events, _, _, err := MutationEvents(context.Background(), dir, []mutate.Record{
		recordOf(ms[0], first, "survived"), recordOf(ms[1], second, "survived"),
	})
	if err != nil {
		t.Fatal(err)
	}
	times := map[string]time.Time{}
	for _, ev := range events {
		times[ev.BaseSHA] = ev.Time
	}
	if !times[first].Equal(baseTime) || !times[second].Equal(later) {
		t.Errorf("times = %v, want %v for %s and %v for %s", times, baseTime, first, later, second)
	}
}

func TestMutationEventsErrors(t *testing.T) {
	dir, base := newRepo(t, mutationFiles())
	ms := cmpMutants(t)
	good := recordOf(ms[0], base, "survived")
	cases := map[string]struct {
		recs []mutate.Record
		want string // substring of the error
	}{
		"unknown mutant ID": {
			recs: []mutate.Record{good, {ID: "deadbeefdeadbeef", File: "calc/calc.go", Operator: "cmp", BaseSHA: base, Status: "killed", Killers: []string{"t"}}},
			want: "deadbeefdeadbeef",
		},
		"file absent at the base": {
			recs: []mutate.Record{{ID: "feedfeedfeedfeed", File: "calc/missing.go", Operator: "cmp", BaseSHA: base, Status: "survived"}},
			want: "feedfeedfeedfeed",
		},
		"base not in the repository": {
			recs: []mutate.Record{{ID: good.ID, File: good.File, Operator: "cmp", BaseSHA: strings.Repeat("0", 40), Status: "survived"}},
			want: good.ID,
		},
		"killed without killers": {
			recs: []mutate.Record{recordOf(ms[0], base, "killed")},
			want: ms[0].ID,
		},
		"duplicate ID": {
			recs: []mutate.Record{good, good},
			want: good.ID,
		},
		"operator that does not produce the ID": {
			recs: []mutate.Record{{ID: good.ID, File: good.File, Operator: "logic", BaseSHA: base, Status: "survived"}},
			want: good.ID,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			events, contents, skipped, err := MutationEvents(context.Background(), dir, tc.recs)
			if err == nil {
				t.Fatalf("MutationEvents = %d events, %d contents, %v, want an error", len(events), len(contents), skipped)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestMutationEventsOfNoRecords(t *testing.T) {
	dir, _ := newRepo(t, mutationFiles())
	events, contents, skipped, err := MutationEvents(context.Background(), dir, nil)
	if err != nil || len(events) != 0 || len(contents) != 0 || len(skipped) != 0 {
		t.Errorf("MutationEvents(nil) = %v, %v, %v, %v, want nothing and no error", events, contents, skipped, err)
	}
}
