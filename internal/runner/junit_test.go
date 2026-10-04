package runner

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseJUnitStatusAndMetadata(t *testing.T) {
	f, err := os.Open("../../testdata/junit/basic.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cases, err := ParseJUnit(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatalf("case count: %d", len(cases))
	}
	pass := cases[0]
	if pass.Suite != "suite" || pass.Name != "TestPass" || pass.Classname != "example.com/project/pkg" || pass.File != "pkg_test.go" || pass.Status != "pass" || pass.DurationMS != nil {
		t.Fatalf("raw metadata or unknown duration lost: %+v", pass)
	}
	if !reflect.DeepEqual(pass.Properties, map[string]string{"environment": "linux", "case-key": "case-value"}) {
		t.Fatalf("properties lost: %+v", pass.Properties)
	}
	fail := cases[1]
	if fail.Status != "fail" || fail.DurationMS == nil || *fail.DurationMS != 1235 || fail.Message != "assertion failed\nexpected 1, got 2" || fail.Type != "AssertionError" {
		t.Fatalf("failure metadata or rounded duration lost: %+v", fail)
	}
	mixed := cases[2]
	if mixed.Status != "error" || mixed.DurationMS == nil || *mixed.DurationMS != 0 || mixed.Message != "failure message\nfailure body\nerror message\nerror body\nskip reason" || mixed.Type != "AssertionError\nRuntimeError" {
		t.Fatalf("combined diagnostics lost: %+v", mixed)
	}
	for _, tc := range []struct{ elements, status string }{
		{"", "pass"},
		{"<skipped/>", "skip"},
		{"<failure/>", "fail"},
		{"<error/>", "error"},
		{"<failure/><skipped/>", "fail"},
		{"<skipped/><error/>", "error"},
		{"<error/><failure/>", "error"},
		{"<skipped/><failure/><error/>", "error"},
	} {
		t.Run(tc.status+tc.elements, func(t *testing.T) {
			got, err := ParseJUnit(strings.NewReader(`<testsuite><testcase name="n">` + tc.elements + `</testcase></testsuite>`))
			if err != nil || len(got) != 1 || got[0].Status != tc.status {
				t.Fatalf("status precedence: %+v, %v", got, err)
			}
		})
	}
}

func TestParseJUnitNestedAndDuplicateCases(t *testing.T) {
	data, err := os.ReadFile("../../testdata/junit/nested.xml")
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{string(data), strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(string(data)), "<testsuites>"), "</testsuites>")} {
		cases, err := ParseJUnit(strings.NewReader(root))
		if err != nil {
			t.Fatal(err)
		}
		if len(cases) != 3 {
			t.Fatalf("duplicate cases collapsed: %+v", cases)
		}
		if cases[0].Suite != "outer" || cases[1].Suite != "inner" || cases[2].Suite != "outer" {
			t.Fatalf("nested suite ownership lost: %+v", cases)
		}
		for _, c := range cases {
			if c.Name != "same" || c.DurationMS != nil || c.Properties["late"] != "outer-value" {
				t.Fatalf("duplicate metadata or late inherited property lost: %+v", c)
			}
		}
		if cases[1].Properties["environment"] != "inner" || cases[0].Properties["environment"] != "outer" || cases[2].Properties["environment"] != "case" {
			t.Fatalf("property precedence must be case, inner suite, outer suite: %+v", cases)
		}
	}
}

func TestParseJUnitSuiteCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name, report string
		incomplete   bool
	}{
		{"suite error", `<testsuite><testcase name="n"/><error message="collection failed"/></testsuite>`, true},
		{"missing cases", `<testsuite tests="2"><testcase name="n"/></testsuite>`, true},
		{"extra cases", `<testsuite tests="0"><testcase name="n"/></testsuite>`, true},
		{"unreported error", `<testsuite tests="1" errors="1"><testcase name="n"/></testsuite>`, true},
		{"complete case error", `<testsuite tests="1" errors="1"><testcase name="n"><error/></testcase></testsuite>`, false},
		{"complete", `<testsuite tests="1" failures="0" skipped="0"><testcase name="n"/></testsuite>`, false},
		{"parent missing case", `<testsuite tests="2"><testsuite tests="1"><testcase name="n"/></testsuite></testsuite>`, true},
		{"empty child suite error", `<testsuite><testcase name="n"/><testsuite><error message="collection failed"/></testsuite></testsuite>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cases, err := ParseJUnit(strings.NewReader(tc.report))
			if err != nil || len(cases) != 1 {
				t.Fatalf("unexpected result: %+v %v", cases, err)
			}
			if cases[0].SuiteIncomplete != tc.incomplete {
				t.Fatalf("suite completeness: %+v; want incomplete=%v", cases[0], tc.incomplete)
			}
			if tc.name != "complete case error" && cases[0].Status != "pass" {
				t.Fatalf("suite problem invented testcase failure: %+v", cases[0])
			}
		})
	}
}

func TestParseJUnitInvalidInput(t *testing.T) {
	for name, report := range map[string]string{
		"empty":                      "",
		"wrong root":                 `<report><testsuite><testcase name="n"/></testsuite></report>`,
		"truncated XML":              `<testsuite><testcase name="n"/>`,
		"unknown entity":             `<testsuite><testcase name="n">&unknown;</testcase></testsuite>`,
		"external entity":            `<!DOCTYPE testsuite [<!ENTITY x SYSTEM "file:///secret">]><testsuite><testcase name="n">&x;</testcase></testsuite>`,
		"external directive":         `<!DOCTYPE testsuite SYSTEM "https://example.invalid/junit.dtd"><testsuite/>`,
		"negative duration":          `<testsuite><testcase name="n" time="-1"/></testsuite>`,
		"NaN duration":               `<testsuite><testcase name="n" time="NaN"/></testsuite>`,
		"infinite duration":          `<testsuite><testcase name="n" time="+Inf"/></testsuite>`,
		"overflow duration":          `<testsuite><testcase name="n" time="9223372036854776"/></testsuite>`,
		"invalid duration":           `<testsuite><testcase name="n" time="oops"/></testsuite>`,
		"empty duration":             `<testsuite><testcase name="n" time=""/></testsuite>`,
		"invalid count":              `<testsuite tests="oops"><testcase name="n"/></testsuite>`,
		"negative count":             `<testsuite tests="-1"><testcase name="n"/></testsuite>`,
		"fractional count":           `<testsuite errors="1.5"><testcase name="n"/></testsuite>`,
		"invalid suite duration":     `<testsuite time="-1"><testcase name="n"/></testsuite>`,
		"second root":                `<testsuite><testcase name="n"/></testsuite><testsuite/>`,
		"outside root text":          `text<testsuite/>`,
		"trailing text":              `<testsuite/>text`,
		"case outside suite":         `<testsuites><testcase name="n"/></testsuites>`,
		"nested case":                `<testsuite><testcase name="n"><testcase name="m"/></testcase></testsuite>`,
		"suite inside case":          `<testsuite><testcase name="n"><testsuite/></testcase></testsuite>`,
		"partial valid then invalid": `<testsuite><testcase name="valid"/><testcase name="invalid" time="-1"/></testsuite>`,
	} {
		t.Run(name, func(t *testing.T) {
			cases, err := ParseJUnit(strings.NewReader(report))
			if err == nil || cases != nil {
				t.Fatalf("invalid report returned partial success: cases=%+v error=%v", cases, err)
			}
		})
	}
}

func TestParseJUnitDiagnosticBound(t *testing.T) {
	report := `<testsuite><testcase name="n"><failure message="first">` + strings.Repeat("x", 65536) + `</failure><error message="last"/></testcase></testsuite>`
	cases, err := ParseJUnit(strings.NewReader(report))
	if err != nil || len(cases) != 1 {
		t.Fatalf("bounded diagnostic parse: %+v %v", cases, err)
	}
	if len(cases[0].Message) > 65536 || !strings.HasPrefix(cases[0].Message, "first\nx") || cases[0].Status != "error" {
		t.Fatalf("diagnostic did not stay bounded while preserving precedence: length=%d status=%s", len(cases[0].Message), cases[0].Status)
	}
}

func TestParseJUnitDiagnosticWhitespace(t *testing.T) {
	cases, err := ParseJUnit(strings.NewReader("<testsuite><testcase name=\"n\"><failure message=\"message\"> \n detail \n </failure></testcase></testsuite>"))
	if err != nil || len(cases) != 1 || cases[0].Message != "message\n \n detail \n " {
		t.Fatalf("diagnostic body whitespace lost: %+v %v", cases, err)
	}
}

func TestParseJUnitDiagnosticTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic string
		truncated        bool
	}{
		{"complete", `<failure message="short" type="AssertionError">body</failure>`, false},
		{"exact message bound", `<failure message="` + strings.Repeat("x", 65536) + `"/>`, false},
		{"message attribute", `<failure message="` + strings.Repeat("x", 65537) + `"/>`, true},
		{"body", `<failure>` + strings.Repeat("x", 65537) + `</failure>`, true},
		{"unicode body", `<failure>` + strings.Repeat("€", 21846) + `</failure>`, true},
		{"combined", `<failure message="first">` + strings.Repeat("x", 65536) + `</failure>`, true},
		{"type", `<error type="` + strings.Repeat("x", 65537) + `"/>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cases, err := ParseJUnit(strings.NewReader(`<testsuite><testcase name="n">` + tc.diagnostic + `</testcase></testsuite>`))
			if err != nil || len(cases) != 1 {
				t.Fatalf("parse: %+v %v", cases, err)
			}
			if cases[0].DiagnosticsTruncated != tc.truncated || cases[0].SuiteIncomplete {
				t.Fatalf("truncation must stay explicit and separate from completeness: truncated=%v incomplete=%v", cases[0].DiagnosticsTruncated, cases[0].SuiteIncomplete)
			}
			if !utf8.ValidString(cases[0].Message) || !utf8.ValidString(cases[0].Type) {
				t.Fatal("diagnostic truncation corrupted UTF-8")
			}
			if len(cases[0].Message) > 65536 || len(cases[0].Type) > 65536 {
				t.Fatalf("diagnostic escaped bound: message=%d type=%d", len(cases[0].Message), len(cases[0].Type))
			}
		})
	}
}

func TestParseJUnitPropertyBudget(t *testing.T) {
	var properties strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&properties, `<property name="p%d" value="value"/>`, i)
	}
	for _, count := range []int{999, 1000} {
		report := `<testsuite><properties>` + properties.String() + `</properties>` + strings.Repeat(`<testcase name="n"/>`, count) + `</testsuite>`
		cases, err := ParseJUnit(strings.NewReader(report))
		if count == 999 && (err != nil || len(cases) != count || len(cases[0].Properties) != 1000) {
			t.Fatalf("supported property boundary rejected: cases=%d error=%v", len(cases), err)
		}
		if count == 1000 && (err == nil || cases != nil) {
			t.Fatalf("property budget allowed inherited amplification: cases=%d error=%v", len(cases), err)
		}
	}
}

func TestParseJUnitLimits(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		const limit = 32 << 20
		r := &junitCountingReader{reader: io.MultiReader(strings.NewReader("<testsuite><!--"), &junitPaddingReader{remaining: limit}, strings.NewReader("--></testsuite>"))}
		cases, err := ParseJUnit(r)
		if err == nil || cases != nil {
			t.Fatalf("byte limit allowed oversized report: cases=%+v error=%v", cases, err)
		}
		if r.bytes > limit+1 {
			t.Fatalf("byte limit kept reading after bound: read %d bytes, maximum %d", r.bytes, limit+1)
		}
	})
	t.Run("cases", func(t *testing.T) {
		for _, n := range []int{100000, 100001} {
			r := strings.NewReader("<testsuite>" + strings.Repeat(`<testcase name="n"/>`, n) + "</testsuite>")
			cases, err := ParseJUnit(r)
			if n == 100000 && (err != nil || len(cases) != n) {
				t.Fatalf("supported testcase boundary rejected: count=%d error=%v", len(cases), err)
			}
			if n == 100001 && (err == nil || cases != nil) {
				t.Fatalf("testcase limit allowed oversized report: count=%d error=%v", len(cases), err)
			}
		}
	})
}

type junitCountingReader struct {
	reader io.Reader
	bytes  int
}

func (r *junitCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}

type junitPaddingReader struct{ remaining int }

func (r *junitPaddingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}
