package runner

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// goEvent is one test2json event as printed by go test -json.
type goEvent struct {
	Action      string
	Package     string
	ImportPath  string
	Test        string
	Elapsed     float64
	Output      string
	FailedBuild string
}

// goJUnitTailBytes bounds the output kept per test and per package.
const goJUnitTailBytes = 8 << 10

// tailBuffer keeps the last goJUnitTailBytes bytes written to it.
type tailBuffer struct {
	data      []byte
	truncated bool
}

func (t *tailBuffer) write(s string) {
	t.data = append(t.data, s...)
	if len(t.data) > 2*goJUnitTailBytes {
		t.data = append(t.data[:0], t.data[len(t.data)-goJUnitTailBytes:]...)
		t.truncated = true
	}
}

func (t *tailBuffer) String() string {
	data, truncated := t.data, t.truncated
	if len(data) > goJUnitTailBytes {
		data, truncated = data[len(data)-goJUnitTailBytes:], true
	}
	for len(data) > 0 && !utf8.RuneStart(data[0]) {
		data = data[1:]
	}
	if truncated {
		return "[output truncated]\n" + string(data)
	}
	return string(data)
}

type goCaseResult struct {
	name    string
	status  string // pass, fail, skip, error
	elapsed float64
	seq     int
	out     tailBuffer
}

type goPackageResult struct {
	name        string
	finished    []*goCaseResult
	active      map[string]*goCaseResult
	seq         int
	result      string // "" until the package reports pass, fail or skip
	elapsed     float64
	failedBuild string
	out         tailBuffer
	started     map[string]bool // tests that have a run event, subtests included
}

// goJUnitBuilder turns a go test -json stream into one JUnit report. It is an
// io.Writer over the raw stream and always consumes all of its input: a failing
// human writer is recorded in Err instead of stopping the copy, which would
// leave the child process blocked on a full pipe.
type goJUnitBuilder struct {
	human    io.Writer
	pending  []byte
	packages map[string]*goPackageResult
	builds   map[string]*tailBuffer
	humanErr error
}

func newGoJUnitBuilder(human io.Writer) *goJUnitBuilder {
	if human == nil {
		human = io.Discard
	}
	return &goJUnitBuilder{human: human, packages: map[string]*goPackageResult{}, builds: map[string]*tailBuffer{}}
}

// Err returns the first error from the human output writer.
func (b *goJUnitBuilder) Err() error { return b.humanErr }

func (b *goJUnitBuilder) Write(p []byte) (int, error) {
	b.pending = append(b.pending, p...)
	for {
		i := bytes.IndexByte(b.pending, '\n')
		if i < 0 {
			return len(p), nil
		}
		b.line(b.pending[:i], true)
		b.pending = b.pending[i+1:]
	}
}

// Flush handles a final line that has no trailing newline.
func (b *goJUnitBuilder) Flush() {
	if len(b.pending) > 0 {
		b.line(b.pending, false)
	}
	b.pending = nil
}

func (b *goJUnitBuilder) forward(s string) {
	if s == "" || b.humanErr != nil {
		return
	}
	if _, err := io.WriteString(b.human, s); err != nil {
		b.humanErr = err
	}
}

func (b *goJUnitBuilder) line(raw []byte, newline bool) {
	trimmed := bytes.TrimSpace(raw)
	var ev goEvent
	if len(trimmed) > 0 && trimmed[0] == '{' && json.Unmarshal(trimmed, &ev) == nil && ev.Action != "" {
		b.handle(ev)
		return
	}
	// Not a test2json event: pass it through for the human.
	b.forward(string(raw))
	if newline {
		b.forward("\n")
	}
}

// started reports whether the stream held a run event for the test of the
// package: the test body was entered, whatever its result.
func (b *goJUnitBuilder) started(pkg, test string) bool {
	p := b.packages[pkg]
	return p != nil && p.started[test]
}

func (b *goJUnitBuilder) pkg(name string) *goPackageResult {
	p := b.packages[name]
	if p == nil {
		p = &goPackageResult{name: name, active: map[string]*goCaseResult{}, started: map[string]bool{}}
		b.packages[name] = p
	}
	return p
}

func (p *goPackageResult) testCase(name string) *goCaseResult {
	c := p.active[name]
	if c == nil {
		p.seq++
		c = &goCaseResult{name: name, seq: p.seq}
		p.active[name] = c
	}
	return c
}

func (p *goPackageResult) finish(name, status string, elapsed float64) {
	c := p.testCase(name)
	c.status, c.elapsed = status, elapsed
	delete(p.active, name)
	p.finished = append(p.finished, c)
}

func (b *goJUnitBuilder) handle(ev goEvent) {
	switch ev.Action {
	case "build-output":
		buf := b.builds[ev.ImportPath]
		if buf == nil {
			buf = &tailBuffer{}
			b.builds[ev.ImportPath] = buf
		}
		buf.write(ev.Output)
		b.forward(ev.Output)
	case "output":
		b.forward(ev.Output)
		if ev.Package == "" {
			return
		}
		p := b.pkg(ev.Package)
		if ev.Test == "" {
			p.out.write(ev.Output)
		} else {
			p.testCase(ev.Test).out.write(ev.Output)
		}
	case "start":
		if ev.Package != "" {
			b.pkg(ev.Package)
		}
	case "run":
		if ev.Package == "" || ev.Test == "" {
			return
		}
		p := b.pkg(ev.Package)
		p.started[ev.Test] = true
		if _, running := p.active[ev.Test]; running {
			// The previous run of this test never reported a result.
			p.finish(ev.Test, "error", 0)
		}
		p.testCase(ev.Test)
	case "pass", "fail", "skip":
		if ev.Package == "" {
			return
		}
		p := b.pkg(ev.Package)
		if ev.Test != "" {
			p.finish(ev.Test, ev.Action, ev.Elapsed)
			return
		}
		p.result, p.elapsed, p.failedBuild = ev.Action, ev.Elapsed, ev.FailedBuild
	}
}

type xmlJUnitSuites struct {
	XMLName xml.Name        `xml:"testsuites"`
	Suites  []xmlJUnitSuite `xml:"testsuite"`
}

type xmlJUnitSuite struct {
	Name     string         `xml:"name,attr"`
	Tests    int            `xml:"tests,attr"`
	Failures int            `xml:"failures,attr"`
	Errors   int            `xml:"errors,attr"`
	Skipped  int            `xml:"skipped,attr"`
	Time     string         `xml:"time,attr"`
	Cases    []xmlJUnitCase `xml:"testcase"`
	Error    *xmlJUnitDiag  `xml:"error,omitempty"`
}

type xmlJUnitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *xmlJUnitDiag `xml:"failure,omitempty"`
	Error     *xmlJUnitDiag `xml:"error,omitempty"`
	Skipped   *xmlJUnitDiag `xml:"skipped,omitempty"`
}

type xmlJUnitDiag struct {
	Message string `xml:"message,attr,omitempty"`
	Body    string `xml:",chardata"`
}

func junitSeconds(s float64) string {
	if s < 0 {
		s = 0
	}
	return fmt.Sprintf("%.3f", s)
}

// Marshal renders the JUnit XML for everything written so far. A test that
// never reported a result is an error case, and a package that failed without a
// failing test, or never reported a result, gets a suite level error so the
// report cannot be read as green.
func (b *goJUnitBuilder) Marshal() ([]byte, error) {
	names := make([]string, 0, len(b.packages))
	for name := range b.packages {
		names = append(names, name)
	}
	slices.Sort(names)

	report := xmlJUnitSuites{}
	for _, name := range names {
		p := b.packages[name]
		cases := slices.Clone(p.finished)
		unfinished := make([]*goCaseResult, 0, len(p.active))
		for _, c := range p.active {
			unfinished = append(unfinished, c)
		}
		slices.SortFunc(unfinished, func(x, y *goCaseResult) int { return x.seq - y.seq })
		for _, c := range unfinished {
			cp := *c
			cp.status = "error"
			cases = append(cases, &cp)
		}

		suite := xmlJUnitSuite{Name: name, Time: junitSeconds(p.elapsed)}
		failing := false
		for _, c := range cases {
			xc := xmlJUnitCase{Name: c.name, Classname: name, Time: junitSeconds(c.elapsed)}
			body := c.out.String()
			switch c.status {
			case "fail":
				suite.Failures++
				failing = true
				xc.Failure = &xmlJUnitDiag{Message: "FAIL", Body: body}
			case "error":
				suite.Errors++
				failing = true
				xc.Error = &xmlJUnitDiag{Message: "test did not report a result", Body: body}
			case "skip":
				suite.Skipped++
				xc.Skipped = &xmlJUnitDiag{Body: body}
			}
			suite.Cases = append(suite.Cases, xc)
		}
		suite.Tests = len(cases)

		switch {
		case p.result == "":
			suite.Error = &xmlJUnitDiag{Message: "package did not report a result", Body: p.out.String()}
		case p.result == "fail" && !failing:
			var sb strings.Builder
			if build := b.builds[p.failedBuild]; p.failedBuild != "" && build != nil {
				sb.WriteString(build.String())
			}
			sb.WriteString(p.out.String())
			suite.Error = &xmlJUnitDiag{Message: "package failed without a failing test", Body: sb.String()}
		}
		report.Suites = append(report.Suites, suite)
	}

	body, err := xml.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render JUnit XML: %w", err)
	}
	return append([]byte(xml.Header), append(body, '\n')...), nil
}
