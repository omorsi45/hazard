package runner

import (
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// JUnitCase preserves report metadata independently of runnable test identity.
type JUnitCase struct {
	Suite, Name, Classname, File, Status string
	DurationMS                           *int64
	Message, Type                        string
	Properties                           map[string]string
	SuiteIncomplete                      bool
	DiagnosticsTruncated                 bool
}

const maxJUnitSuiteDepth = 256

// ParseJUnit reads a JUnit report without inferring missing observations.
func ParseJUnit(r io.Reader) ([]JUnitCase, error) {
	limited := &io.LimitedReader{R: r, N: (32 << 20) + 1}
	d := xml.NewDecoder(limited)
	var cases []JUnitCase
	var suites []junitSuite
	var current *JUnitCase
	var diagnostic *junitDiagnostic
	var elements []string
	rootSeen := false
	reportIncomplete := false
	propertyEntries := 0
	for {
		token, err := d.Token()
		if limited.N == 0 {
			return nil, fmt.Errorf("JUnit report exceeds 32 MiB")
		}
		if err == io.EOF {
			if !rootSeen || len(elements) != 0 {
				return nil, fmt.Errorf("missing or incomplete JUnit root")
			}
			if len(cases) == 0 && reportIncomplete {
				return nil, fmt.Errorf("incomplete JUnit report contains no testcases")
			}
			return cases, nil
		}
		if err != nil {
			return nil, fmt.Errorf("JUnit XML: %w", err)
		}
		switch token := token.(type) {
		case xml.Directive:
			return nil, fmt.Errorf("JUnit XML directives are unsupported")
		case xml.ProcInst:
			if token.Target != "xml" || rootSeen {
				return nil, fmt.Errorf("unexpected JUnit processing instruction")
			}
		case xml.StartElement:
			parent := ""
			if len(elements) > 0 {
				parent = elements[len(elements)-1]
			} else {
				if rootSeen || (token.Name.Local != "testsuite" && token.Name.Local != "testsuites") {
					return nil, fmt.Errorf("unexpected JUnit root %q", token.Name.Local)
				}
				rootSeen = true
			}
			elements = append(elements, token.Name.Local)
			switch token.Name.Local {
			case "testsuite", "testsuites":
				if parent != "" && parent != "testsuite" && parent != "testsuites" {
					return nil, fmt.Errorf("JUnit suite outside suite container")
				}
				if len(suites) >= maxJUnitSuiteDepth {
					return nil, fmt.Errorf("JUnit report exceeds 256 nested suite/container frames")
				}
				name, _ := junitAttribute(token, "name")
				suite := junitSuite{name: name, start: len(cases), properties: map[string]string{}, expected: map[string]int{}}
				if value, present := junitAttribute(token, "time"); present {
					if _, err := junitDuration(value); err != nil {
						return nil, err
					}
				}
				for _, field := range []string{"tests", "errors", "failures", "skipped"} {
					if value, present := junitAttribute(token, field); present {
						count, err := strconv.Atoi(value)
						if err != nil || count < 0 {
							return nil, fmt.Errorf("invalid JUnit %s count %q", field, value)
						}
						suite.expected[field] = count
					}
				}
				suites = append(suites, suite)
			case "testcase":
				if parent != "testsuite" || current != nil {
					return nil, fmt.Errorf("JUnit testcase outside suite or nested in testcase")
				}
				if len(cases) >= 100000 {
					return nil, fmt.Errorf("JUnit report exceeds 100000 testcases")
				}
				current = &JUnitCase{Suite: suites[len(suites)-1].name, Status: "pass", Properties: map[string]string{}}
				current.Name, _ = junitAttribute(token, "name")
				current.Classname, _ = junitAttribute(token, "classname")
				current.File, _ = junitAttribute(token, "file")
				if value, present := junitAttribute(token, "time"); present {
					ms, err := junitDuration(value)
					if err != nil {
						return nil, err
					}
					current.DurationMS = &ms
				}
			case "property":
				if parent != "properties" || len(elements) < 3 {
					continue
				}
				owner := elements[len(elements)-3]
				name, _ := junitAttribute(token, "name")
				value, _ := junitAttribute(token, "value")
				if owner == "testcase" && current != nil {
					if err := setJUnitProperty(current.Properties, name, value, &propertyEntries); err != nil {
						return nil, err
					}
				} else if owner == "testsuite" || owner == "testsuites" {
					if err := setJUnitProperty(suites[len(suites)-1].properties, name, value, &propertyEntries); err != nil {
						return nil, err
					}
				}
			case "error", "failure", "skipped":
				if parent == "testcase" && current != nil {
					diagnostic = &junitDiagnostic{depth: len(elements)}
					diagnostic.message, _ = junitAttribute(token, "message")
					diagnostic.kind, _ = junitAttribute(token, "type")
					if token.Name.Local == "error" {
						current.Status = "error"
					} else if token.Name.Local == "failure" && current.Status != "error" {
						current.Status = "fail"
					} else if token.Name.Local == "skipped" && current.Status == "pass" {
						current.Status = "skip"
					}
				} else if token.Name.Local == "error" && (parent == "testsuite" || parent == "testsuites") {
					suites[len(suites)-1].incomplete = true
				}
			}
		case xml.CharData:
			if len(elements) == 0 && strings.TrimSpace(string(token)) != "" {
				return nil, fmt.Errorf("text outside JUnit root")
			}
			if diagnostic != nil {
				n := min(len(token), junitDiagnosticLimit-diagnostic.body.Len())
				if n < len(token) {
					current.DiagnosticsTruncated = true
				}
				diagnostic.body.Write(token[:n])
			}
		case xml.EndElement:
			switch token.Name.Local {
			case "testsuite", "testsuites":
				suite := suites[len(suites)-1]
				counts := map[string]int{"tests": len(cases) - suite.start}
				for _, c := range cases[suite.start:] {
					switch c.Status {
					case "error":
						counts["errors"]++
					case "fail":
						counts["failures"]++
					case "skip":
						counts["skipped"]++
					}
				}
				for field, want := range suite.expected {
					if counts[field] != want {
						suite.incomplete = true
					}
				}
				reportIncomplete = reportIncomplete || suite.incomplete
				for i := suite.start; i < len(cases); i++ {
					cases[i].SuiteIncomplete = cases[i].SuiteIncomplete || suite.incomplete
					for k, v := range suite.properties {
						if _, present := cases[i].Properties[k]; !present {
							if err := setJUnitProperty(cases[i].Properties, k, v, &propertyEntries); err != nil {
								return nil, err
							}
						}
					}
				}
				suites = suites[:len(suites)-1]
				if len(suites) > 0 && suite.incomplete {
					suites[len(suites)-1].incomplete = true
				}
			case "testcase":
				cases = append(cases, *current)
				current = nil
			case "error", "failure", "skipped":
				if diagnostic != nil && diagnostic.depth == len(elements) {
					appendJUnitText(current, &current.Message, diagnostic.message)
					appendJUnitText(current, &current.Message, diagnostic.body.String())
					appendJUnitText(current, &current.Type, diagnostic.kind)
					diagnostic = nil
				}
			}
			elements = elements[:len(elements)-1]
		}
	}
}

type junitSuite struct {
	name       string
	start      int
	properties map[string]string
	expected   map[string]int
	incomplete bool
}

type junitDiagnostic struct {
	message, kind string
	body          strings.Builder
	depth         int
}

func junitDuration(value string) (int64, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	ms := math.Round(seconds * 1000)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || ms >= math.Exp2(63) {
		return 0, fmt.Errorf("invalid JUnit duration %q", value)
	}
	return int64(ms), nil
}

func junitAttribute(element xml.StartElement, name string) (string, bool) {
	for _, attr := range element.Attr {
		if attr.Name.Local == name {
			return attr.Value, true
		}
	}
	return "", false
}

const junitDiagnosticLimit = 64 << 10

func appendJUnitText(c *JUnitCase, field *string, text string) {
	if text == "" {
		return
	}
	value := text
	if *field != "" {
		value = *field + "\n" + text
	}
	if len(value) > junitDiagnosticLimit {
		c.DiagnosticsTruncated = true
	}
	*field = truncateJUnitText(value)
}

func truncateJUnitText(value string) string {
	end := min(len(value), junitDiagnosticLimit)
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func setJUnitProperty(properties map[string]string, name, value string, entries *int) error {
	if _, present := properties[name]; !present {
		if *entries >= 1000000 {
			return fmt.Errorf("JUnit report exceeds 1000000 materialized property entries")
		}
		(*entries)++
	}
	properties[name] = value
	return nil
}
