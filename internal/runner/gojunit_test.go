package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func eventLines(t *testing.T, events ...goEvent) string {
	t.Helper()
	var sb strings.Builder
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// junitFromStream feeds a raw stream through the builder in chunks of the given
// size (0 means one write) and parses the resulting report.
func junitFromStream(t *testing.T, stream string, chunk int) ([]JUnitCase, string, string) {
	t.Helper()
	var human bytes.Buffer
	b := newGoJUnitBuilder(&human)
	if chunk <= 0 {
		chunk = len(stream)
	}
	for i := 0; i < len(stream); i += chunk {
		end := min(i+chunk, len(stream))
		if n, err := b.Write([]byte(stream[i:end])); err != nil || n != end-i {
			t.Fatalf("Write: %d, %v", n, err)
		}
	}
	b.Flush()
	data, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := ParseJUnit(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseJUnit: %v\n%s", err, data)
	}
	return cases, string(data), human.String()
}

const pkgA = "example.com/m/a"

func mixedPackageEvents() []goEvent {
	return []goEvent{
		{Action: "start", Package: pkgA},
		{Action: "run", Package: pkgA, Test: "TestPass"},
		{Action: "output", Package: pkgA, Test: "TestPass", Output: "=== RUN   TestPass\n"},
		{Action: "pass", Package: pkgA, Test: "TestPass", Elapsed: 0.01},
		{Action: "run", Package: pkgA, Test: "TestFail"},
		{Action: "output", Package: pkgA, Test: "TestFail", Output: "    a_test.go:10: boom\n"},
		{Action: "output", Package: pkgA, Test: "TestFail", Output: "--- FAIL: TestFail (0.02s)\n"},
		{Action: "fail", Package: pkgA, Test: "TestFail", Elapsed: 0.02},
		{Action: "run", Package: pkgA, Test: "TestSkip"},
		{Action: "output", Package: pkgA, Test: "TestSkip", Output: "    a_test.go:20: not today\n"},
		{Action: "skip", Package: pkgA, Test: "TestSkip", Elapsed: 0},
		{Action: "run", Package: pkgA, Test: "TestParent"},
		{Action: "run", Package: pkgA, Test: "TestParent/sub_name"},
		{Action: "pass", Package: pkgA, Test: "TestParent/sub_name", Elapsed: 0.001},
		{Action: "pass", Package: pkgA, Test: "TestParent", Elapsed: 0.002},
		{Action: "output", Package: pkgA, Output: "FAIL\n"},
		{Action: "fail", Package: pkgA, Elapsed: 0.5},
	}
}

func TestGoJUnitStatusesMetadataAndIdentity(t *testing.T) {
	cases, _, _ := junitFromStream(t, eventLines(t, mixedPackageEvents()...), 0)
	if len(cases) != 5 {
		t.Fatalf("cases: %+v", cases)
	}
	want := []struct {
		name, status string
		ms           int64
	}{
		{"TestPass", "pass", 10},
		{"TestFail", "fail", 20},
		{"TestSkip", "skip", 0},
		{"TestParent/sub_name", "pass", 1},
		{"TestParent", "pass", 2},
	}
	for i, w := range want {
		c := cases[i]
		if c.Name != w.name || c.Status != w.status || c.Classname != pkgA || c.Suite != pkgA {
			t.Errorf("case %d: %+v, want %+v", i, c, w)
		}
		if c.DurationMS == nil || *c.DurationMS != w.ms {
			t.Errorf("case %d duration: %v, want %d", i, c.DurationMS, w.ms)
		}
		if c.SuiteIncomplete {
			t.Errorf("case %d: healthy report marked incomplete", i)
		}
		id, err := NormalizeJUnitCase("go", pkgA, c)
		if err != nil || id.ID != "go:"+pkgA+":"+w.name {
			t.Errorf("case %d identity: %+v %v", i, id, err)
		}
	}
	if !strings.Contains(cases[1].Message, "a_test.go:10: boom") || !strings.Contains(cases[1].Message, "--- FAIL: TestFail") {
		t.Errorf("failure output tail missing: %q", cases[1].Message)
	}
	if !strings.Contains(cases[2].Message, "not today") {
		t.Errorf("skip reason missing: %q", cases[2].Message)
	}
	if cases[0].Message != "" || cases[3].Message != "" {
		t.Errorf("passing tests must carry no diagnostics: %q %q", cases[0].Message, cases[3].Message)
	}
}

func TestGoJUnitHandlesChunkedWritesAndForwardsHumanOutput(t *testing.T) {
	stream := eventLines(t, mixedPackageEvents()...) + "not json at all\n" + `{"Action":"output","Package":"` + pkgA + `","Output":"tail without newline"}`
	for _, chunk := range []int{0, 1, 7, 64} {
		cases, _, human := junitFromStream(t, stream, chunk)
		if len(cases) != 5 {
			t.Fatalf("chunk %d: cases %d", chunk, len(cases))
		}
		want := "=== RUN   TestPass\n    a_test.go:10: boom\n--- FAIL: TestFail (0.02s)\n    a_test.go:20: not today\nFAIL\nnot json at all\ntail without newline"
		if human != want {
			t.Fatalf("chunk %d human output:\n got %q\nwant %q", chunk, human, want)
		}
	}
}

func TestGoJUnitSuitesAreSortedByPackage(t *testing.T) {
	stream := eventLines(t,
		goEvent{Action: "run", Package: "example.com/m/z", Test: "TestZ"},
		goEvent{Action: "pass", Package: "example.com/m/z", Test: "TestZ"},
		goEvent{Action: "pass", Package: "example.com/m/z"},
		goEvent{Action: "run", Package: "example.com/m/b", Test: "TestB"},
		goEvent{Action: "pass", Package: "example.com/m/b", Test: "TestB"},
		goEvent{Action: "pass", Package: "example.com/m/b"},
	)
	cases, _, _ := junitFromStream(t, stream, 0)
	if len(cases) != 2 || cases[0].Suite != "example.com/m/b" || cases[1].Suite != "example.com/m/z" {
		t.Fatalf("cases: %+v", cases)
	}
}

func TestGoJUnitRepeatedRunsAreSeparateCases(t *testing.T) {
	stream := eventLines(t,
		goEvent{Action: "run", Package: pkgA, Test: "TestA"},
		goEvent{Action: "pass", Package: pkgA, Test: "TestA", Elapsed: 0.1},
		goEvent{Action: "run", Package: pkgA, Test: "TestA"},
		goEvent{Action: "output", Package: pkgA, Test: "TestA", Output: "second run broke\n"},
		goEvent{Action: "fail", Package: pkgA, Test: "TestA", Elapsed: 0.1},
		goEvent{Action: "fail", Package: pkgA},
	)
	cases, _, _ := junitFromStream(t, stream, 0)
	if len(cases) != 2 || cases[0].Status != "pass" || cases[1].Status != "fail" || !strings.Contains(cases[1].Message, "second run broke") {
		t.Fatalf("-count=2 collapsed or lost the failing run: %+v", cases)
	}
}

func TestGoJUnitTestWithoutResultIsAnError(t *testing.T) {
	stream := eventLines(t,
		goEvent{Action: "run", Package: pkgA, Test: "TestOK"},
		goEvent{Action: "pass", Package: pkgA, Test: "TestOK"},
		goEvent{Action: "run", Package: pkgA, Test: "TestHang"},
		goEvent{Action: "output", Package: pkgA, Test: "TestHang", Output: "panic: test timed out after 1s\n"},
		goEvent{Action: "output", Package: pkgA, Output: "FAIL\n"},
		goEvent{Action: "fail", Package: pkgA, Elapsed: 1},
	)
	cases, _, _ := junitFromStream(t, stream, 0)
	if len(cases) != 2 || cases[1].Name != "TestHang" || cases[1].Status != "error" {
		t.Fatalf("a test that never reported a result must be an error: %+v", cases)
	}
	if !strings.Contains(cases[1].Message, "did not report a result") || !strings.Contains(cases[1].Message, "panic: test timed out") {
		t.Fatalf("message: %q", cases[1].Message)
	}
}

func TestGoJUnitPackageFailureWithoutFailingTestIsNotGreen(t *testing.T) {
	build := []goEvent{
		{Action: "build-output", ImportPath: "example.com/m/a [example.com/m/a.test]", Output: "a_test.go:5:2: undefined: nope\n"},
		{Action: "build-fail", ImportPath: "example.com/m/a [example.com/m/a.test]"},
		{Action: "start", Package: pkgA},
		{Action: "output", Package: pkgA, Output: "FAIL\texample.com/m/a [build failed]\n"},
		{Action: "fail", Package: pkgA, FailedBuild: "example.com/m/a [example.com/m/a.test]"},
	}
	// Alone, the report has no test cases and an incomplete suite: ParseJUnit refuses it.
	var human bytes.Buffer
	b := newGoJUnitBuilder(&human)
	if _, err := b.Write([]byte(eventLines(t, build...))); err != nil {
		t.Fatal(err)
	}
	data, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJUnit(bytes.NewReader(data)); err == nil {
		t.Fatalf("a build-failed package alone parsed as a valid report:\n%s", data)
	}
	if !strings.Contains(string(data), "undefined: nope") || !strings.Contains(string(data), "[build failed]") {
		t.Fatalf("build output missing from the report:\n%s", data)
	}
	if !strings.Contains(human.String(), "undefined: nope") {
		t.Fatalf("build output not forwarded to the human stream: %q", human.String())
	}

	// Next to a healthy package, the failed suite marks the whole report
	// incomplete, so no case of it can be read as a trustworthy green.
	other := []goEvent{
		{Action: "run", Package: "example.com/m/b", Test: "TestB"},
		{Action: "pass", Package: "example.com/m/b", Test: "TestB"},
		{Action: "pass", Package: "example.com/m/b"},
	}
	cases, xmlText, _ := junitFromStream(t, eventLines(t, append(build, other...)...), 0)
	if len(cases) != 1 || cases[0].Suite != "example.com/m/b" || !cases[0].SuiteIncomplete {
		t.Fatalf("report with a failed package must be incomplete: %+v\n%s", cases, xmlText)
	}
	if !strings.Contains(xmlText, "<error") {
		t.Fatalf("failed package has no error marker:\n%s", xmlText)
	}
}

func TestGoJUnitMissingPackageResultMarksSuiteIncomplete(t *testing.T) {
	stream := eventLines(t,
		goEvent{Action: "run", Package: pkgA, Test: "TestA"},
		goEvent{Action: "pass", Package: pkgA, Test: "TestA"},
		goEvent{Action: "run", Package: pkgA, Test: "TestB"},
	)
	cases, _, _ := junitFromStream(t, stream, 0)
	if len(cases) != 2 {
		t.Fatalf("cases: %+v", cases)
	}
	for _, c := range cases {
		if !c.SuiteIncomplete {
			t.Errorf("%s: a suite with no package result must be incomplete", c.Name)
		}
	}
	if cases[1].Name != "TestB" || cases[1].Status != "error" {
		t.Errorf("unfinished test: %+v", cases[1])
	}
}

func TestGoJUnitPackageWithoutTestsIsEmptySuite(t *testing.T) {
	stream := eventLines(t,
		goEvent{Action: "output", Package: "example.com/m/gen", Output: "?   \texample.com/m/gen\t[no test files]\n"},
		goEvent{Action: "skip", Package: "example.com/m/gen"},
		goEvent{Action: "run", Package: pkgA, Test: "TestA"},
		goEvent{Action: "pass", Package: pkgA, Test: "TestA"},
		goEvent{Action: "pass", Package: pkgA},
	)
	cases, _, _ := junitFromStream(t, stream, 0)
	if len(cases) != 1 || cases[0].SuiteIncomplete {
		t.Fatalf("a package with no test files must not taint the report: %+v", cases)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

func TestGoJUnitKeepsConsumingWhenHumanWriterFails(t *testing.T) {
	stream := eventLines(t, mixedPackageEvents()...)
	b := newGoJUnitBuilder(failingWriter{})
	// Returning an error from Write would stop the copy from the child's pipe
	// and leave the child blocked, so the builder must record the failure in
	// Err and keep consuming.
	if n, err := b.Write([]byte(stream)); err != nil || n != len(stream) {
		t.Fatalf("Write: %d, %v", n, err)
	}
	if b.Err() == nil {
		t.Fatal("human writer failure was lost")
	}
	data, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := ParseJUnit(bytes.NewReader(data))
	if err != nil || len(cases) != 5 {
		t.Fatalf("report lost after a human writer failure: %d cases, %v", len(cases), err)
	}
}

func TestGoJUnitEmptyStreamIsAnEmptyReport(t *testing.T) {
	cases, data, _ := junitFromStream(t, "", 0)
	if len(cases) != 0 || !strings.Contains(data, "<testsuites") {
		t.Fatalf("cases %+v\n%s", cases, data)
	}
}

func TestGoJUnitSanitizesOutputAndBoundsTail(t *testing.T) {
	huge := strings.Repeat("filler line that is long enough to overflow the tail\n", 4000)
	stream := eventLines(t,
		goEvent{Action: "run", Package: pkgA, Test: "TestNoisy"},
		goEvent{Action: "output", Package: pkgA, Test: "TestNoisy", Output: "bad \x00\x01 bytes and \xff\xfe invalid utf8\n"},
		goEvent{Action: "output", Package: pkgA, Test: "TestNoisy", Output: huge},
		goEvent{Action: "output", Package: pkgA, Test: "TestNoisy", Output: "the final line\n"},
		goEvent{Action: "fail", Package: pkgA, Test: "TestNoisy", Elapsed: 1},
		goEvent{Action: "fail", Package: pkgA, Elapsed: 1},
	)
	cases, _, _ := junitFromStream(t, stream, 0)
	if len(cases) != 1 {
		t.Fatalf("cases: %+v", cases)
	}
	msg := cases[0].Message
	if !strings.HasSuffix(strings.TrimSpace(msg), "the final line") {
		t.Fatalf("tail must end with the last output line, got ...%q", msg[max(0, len(msg)-80):])
	}
	if len(msg) > 16<<10 {
		t.Fatalf("tail is not bounded: %d bytes", len(msg))
	}
	if !strings.Contains(msg, "output truncated") {
		t.Fatalf("truncation is not marked: %q", msg[:80])
	}
}
