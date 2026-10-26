package mutate

import (
	"reflect"
	"strings"
	"testing"
)

func jsonLines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

func TestParseGoTestJSONFailuresAndDurations(t *testing.T) {
	in := jsonLines(
		`{"Action":"start","Package":"m/calc"}`,
		`{"Action":"run","Package":"m/calc","Test":"TestAdd"}`,
		`{"Action":"pass","Package":"m/calc","Test":"TestAdd","Elapsed":0.25}`,
		`{"Action":"run","Package":"m/calc","Test":"TestSub"}`,
		`{"Action":"run","Package":"m/calc","Test":"TestSub/neg"}`,
		`{"Action":"fail","Package":"m/calc","Test":"TestSub/neg","Elapsed":0}`,
		`{"Action":"fail","Package":"m/calc","Test":"TestSub","Elapsed":1.5}`,
		`{"Action":"skip","Package":"m/calc","Test":"TestSkipped","Elapsed":0}`,
		`{"Action":"fail","Package":"m/calc","Elapsed":2}`,
		`{"Action":"fail","Package":"m/app","Test":"TestRun","Elapsed":3}`,
		`{"Action":"fail","Package":"m/app","Elapsed":3}`,
		`{"Action":"pass","Package":"m/other","Elapsed":1}`,
	)
	got, err := parseGoTestJSON(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"go:m/app:TestRun", "go:m/calc:TestSub"}; !reflect.DeepEqual(got.Failed, want) {
		t.Errorf("Failed = %v, want %v (top-level tests only, sorted)", got.Failed, want)
	}
	if want := map[string][]string{"m/app": {"TestRun"}, "m/calc": {"TestSub"}}; !reflect.DeepEqual(got.FailedByPackage, want) {
		t.Errorf("FailedByPackage = %v, want %v", got.FailedByPackage, want)
	}
	if len(got.PackagesFailedWithoutTest) != 0 || got.TimedOut || len(got.BuildFailed) != 0 {
		t.Errorf("unexpected flags: %+v", got)
	}
	if want := map[string]float64{"go:m/calc:TestAdd": 0.25, "go:m/calc:TestSub": 1.5, "go:m/app:TestRun": 3}; !reflect.DeepEqual(got.Durations, want) {
		t.Errorf("Durations = %v, want %v", got.Durations, want)
	}
}

func TestParseGoTestJSONPackageFailureWithoutTest(t *testing.T) {
	in := jsonLines(
		`{"Action":"output","Package":"m/x","Output":"panic: boom\n"}`,
		`{"Action":"fail","Package":"m/x","Elapsed":0.1}`,
		`{"Action":"pass","Package":"m/y","Elapsed":0.1}`,
	)
	got, err := parseGoTestJSON(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Failed) != 0 {
		t.Errorf("Failed = %v, want none", got.Failed)
	}
	if want := []string{"m/x"}; !reflect.DeepEqual(got.PackagesFailedWithoutTest, want) {
		t.Errorf("PackagesFailedWithoutTest = %v, want %v", got.PackagesFailedWithoutTest, want)
	}
}

func TestParseGoTestJSONTimeoutPanic(t *testing.T) {
	in := jsonLines(
		`{"Action":"run","Package":"m/calc","Test":"TestLoop"}`,
		`{"Action":"output","Package":"m/calc","Test":"TestLoop","Output":"panic: test timed out after 20s\n"}`,
		`{"Action":"fail","Package":"m/calc","Elapsed":20.1}`,
	)
	got, err := parseGoTestJSON(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if !got.TimedOut {
		t.Errorf("TimedOut = false, want true: %+v", got)
	}
	in = jsonLines(
		`{"Action":"output","Package":"m/calc","Output":"FAIL\tm/calc\t20.1s\n"}`,
		`{"Action":"output","Package":"m/calc","Output":"panic: test timed out after 1m0s\n"}`,
		`{"Action":"fail","Package":"m/calc","Elapsed":60}`,
	)
	got, err = parseGoTestJSON(strings.NewReader(in))
	if err != nil || !got.TimedOut {
		t.Errorf("package level timeout: TimedOut=%v err=%v", got.TimedOut, err)
	}
}

func TestParseGoTestJSONBuildFailure(t *testing.T) {
	in := jsonLines(
		`{"ImportPath":"m/calc [m/calc.test]","Action":"build-output","Output":"calc.go:3:1: syntax error\n"}`,
		`{"ImportPath":"m/calc [m/calc.test]","Action":"build-fail"}`,
		`{"Action":"start","Package":"m/calc"}`,
		`{"Action":"output","Package":"m/calc","Output":"FAIL\tm/calc [build failed]\n"}`,
		`{"Action":"fail","Package":"m/calc","Elapsed":0}`,
	)
	got, err := parseGoTestJSON(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"m/calc"}; !reflect.DeepEqual(got.BuildFailed, want) {
		t.Errorf("BuildFailed = %v, want %v", got.BuildFailed, want)
	}
	if len(got.PackagesFailedWithoutTest) != 0 {
		t.Errorf("a build failure is reported once, not also as a bare package failure: %v", got.PackagesFailedWithoutTest)
	}
}

func TestParseGoTestJSONNoise(t *testing.T) {
	in := "go: downloading example.com/x v1.0.0\n" + jsonLines(
		`{"Action":"pass","Package":"m/a","Test":"TestA","Elapsed":0.5}`,
		``,
		`{"Action":"pass","Package":"m/a","Elapsed":0.5}`,
	)
	got, err := parseGoTestJSON(strings.NewReader(in))
	if err != nil {
		t.Fatalf("plain text lines must be ignored: %v", err)
	}
	if len(got.Failed) != 0 || got.Durations["go:m/a:TestA"] != 0.5 {
		t.Errorf("got %+v", got)
	}
	if _, err := parseGoTestJSON(strings.NewReader(`{"Action":"fail","Package":` + "\n")); err == nil {
		t.Errorf("a truncated JSON event must be an error, not silently dropped")
	}
}
