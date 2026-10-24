package mine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/runner"
)

func TestMineIdentityLanguage(t *testing.T) {
	duration := int64(12)
	raw := runner.JUnitCase{Suite: "suite", Name: "test_value[param]", Classname: "tests.test_value.Case", File: "tests/test_value.py", Status: "fail", DurationMS: &duration, Message: "original diagnostic", Type: "AssertionError", Properties: map[string]string{"source": "report"}, SuiteIncomplete: true, DiagnosticsTruncated: true}
	for _, tc := range []struct{ name, explicit, producer, module, wantID, wantEvidence, wantError string }{
		{name: "operator_python", explicit: "python", wantID: "py:tests.test_value.Case:test_value[param]", wantEvidence: "operator_language=python"},
		{name: "producer_python", producer: "python", wantID: "py:tests.test_value.Case:test_value[param]", wantEvidence: "report_producer_language=python"},
		{name: "agreeing_sources", explicit: "python", producer: "python", wantID: "py:tests.test_value.Case:test_value[param]", wantEvidence: "operator_language=python;report_producer_language=python"},
		{name: "unknown", producer: "unknown", wantError: "language_unavailable"},
		{name: "conflict", explicit: "python", producer: "go", wantError: "language_conflict"},
		{name: "unsupported_operator", explicit: "ruby", wantError: "unsupported_language"},
		{name: "unsupported_producer", explicit: "python", producer: "ruby", wantError: "unsupported_language"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mineIdentity(tc.explicit, tc.producer, tc.module, raw)
			if !reflect.DeepEqual(got.Case, raw) {
				t.Fatal("mapping changed original report case")
			}
			if got.Identity.ID != tc.wantID || got.IdentityEvidence != tc.wantEvidence || got.IdentityError != tc.wantError {
				t.Fatalf("language mapping: id=%q evidence=%q error=%q", got.Identity.ID, got.IdentityEvidence, got.IdentityError)
			}
			if got.Identity.RunnerID != "" {
				t.Fatal("report mapping invented native RunnerID")
			}
		})
	}
	goCase := runner.JUnitCase{Name: "TestValue/case with space", Classname: "example.org/project", Status: "pass"}
	got := mineIdentity("go", "unknown", "example.org/project", goCase)
	if got.Identity.ID != "go:example.org/project:TestValue/case_with_space" || got.IdentityEvidence != "operator_language=go;tested_root_module_agreement" || got.Identity.RunnerID != "" || !reflect.DeepEqual(got.Case, goCase) {
		t.Fatalf("explicit Go declaration lost mapping or raw case: %+v", got)
	}
	automatic := mineIdentity("", "go", "example.org/project", goCase)
	if automatic.Identity.ID != "go:example.org/project:TestValue/case_with_space" || automatic.IdentityEvidence != "report_producer_language=go;tested_root_module_agreement" {
		t.Fatalf("producer Go declaration unavailable: %+v", automatic)
	}
	missing := mineIdentity("python", "unknown", "", runner.JUnitCase{Name: "test_value"})
	if missing.Identity.ID != "" || missing.IdentityError != "unsupported_test_identity" {
		t.Fatal("missing Python qualifier invented an identity")
	}
}

func TestMineIdentityGoModuleAgreement(t *testing.T) {
	for _, tc := range []struct{ name, module, class, wantID string }{
		{"full_package", "example.org/project", "example.org/project/pkg", "go:example.org/project/pkg:TestValue"},
		{"root_package", "example.org/project", "example.org/project", "go:example.org/project:TestValue"},
		{"single_component_root", "hazard", "hazard", "go:hazard:TestValue"},
		{"lookalike_prefix", "example.org/project", "example.org/project-other/pkg", ""},
		{"outside_module", "example.org/project", "example.org/other/pkg", ""},
		{"short_class", "example.org/project", "pkg", ""},
		{"empty_segment", "example.org/project", "example.org/project//pkg", ""},
		{"dot_segment", "example.org/project", "example.org/project/../pkg", ""},
		{"invalid_class", "example.org/project", "example.org/project/p@kg", ""},
		{"long_path", strings.Repeat("p", 513), strings.Repeat("p", 513), ""},
		{"invalid_module", "example.org/p@kg", "example.org/p@kg", ""},
		{"unavailable_module", "", "example.org/project/pkg", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := runner.JUnitCase{Name: "TestValue", Classname: tc.class, Status: "pass"}
			got := mineIdentity("go", "go", tc.module, raw)
			if got.Identity.ID != tc.wantID {
				t.Fatalf("module agreement identity=%q, want %q", got.Identity.ID, tc.wantID)
			}
			if tc.wantID == "" && (got.IdentityError == "" || got.IdentityEvidence != "") {
				t.Fatal("unavailable module agreement manufactured verified evidence")
			}
			if !reflect.DeepEqual(got.Case, raw) {
				t.Fatal("module rejection changed raw case")
			}
		})
	}
}

func TestMineIdentityModuleReader(t *testing.T) {
	for _, tc := range []struct{ name, source, module, reason string }{
		{"ordinary", "module example.org/project\n\ngo 1.26.6\nrequire (\n example.org/dep v1.0.0 // dependency\n)\n", "example.org/project", ""},
		{"quoted_comments", "/* header */ module \"example.org/project\" // suffix\n", "example.org/project", ""},
		{"single_component", "module hazard\n", "hazard", ""},
		{"duplicate", "module example.org/p\nmodule example.org/p\n", "", "go_module_ambiguous"},
		{"missing", "go 1.26.6\n", "", "go_module_unavailable"},
		{"block_syntax", "module (\n example.org/p\n)\n", "", "go_module_unsupported"},
		{"raw_string", "module `example.org/p`\n", "", "go_module_unsupported"},
		{"malformed_quote", "module \"example.org/p\n", "", "go_module_unsupported"},
		{"malformed_path", "module example.org/p//x\n", "", "go_module_unsupported"},
		{"malformed_comment", "module example.org/p /* unfinished", "", "go_module_unsupported"},
		{"unopened_require", "module example.org/p\n)\n", "", "go_module_unsupported"},
		{"unbalanced_require", "module example.org/p\nrequire (\nexample.org/dep v1.0.0\n", "", "go_module_unsupported"},
		{"extra_token", "module example.org/p other\n", "", "go_module_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readGoModule([]byte(tc.source))
			reason := ""
			if err != nil {
				reason = err.Error()
			}
			if got != tc.module || reason != tc.reason {
				t.Fatalf("module reader: module=%q reason=%q, want %q %q", got, reason, tc.module, tc.reason)
			}
		})
	}
	oversized := append([]byte("module example.org/p\n//"), []byte(strings.Repeat("x", 1<<20))...)
	if module, err := readGoModule(oversized); module != "" || err == nil || err.Error() != "go_module_too_large" {
		t.Fatal("oversized module bytes bypassed bound")
	}
	invalid := []byte{'m', 'o', 'd', 'u', 'l', 'e', ' ', 0xff}
	if module, err := readGoModule(invalid); module != "" || err == nil || err.Error() != "go_module_unsupported" {
		t.Fatal("invalid UTF-8 module input accepted")
	}
}

func TestMineIdentitySyntheticGoErrors(t *testing.T) {
	for _, tc := range []struct {
		name, testName, status, kind, message string
		synthetic                             bool
	}{
		{"build_signature", "build failed", "error", "", "Build error\ncompiler diagnostic", true},
		{"runtime_signature", "Failure", "error", "", "Runtime error\npanic diagnostic", true},
		{"ordinary_failure", "TestBuild", "fail", "", "Build error\nassertion diagnostic", false},
		{"substring", "TestBuild", "error", "", "prefix Build error", false},
		{"type_alone", "TestBuild", "error", "build", "ordinary diagnostic", false},
		{"typed_error", "TestBuild", "error", "AssertionError", "Build error", false},
		{"runtime_name_near_miss", "TestBuild", "error", "", "Runtime error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := runner.JUnitCase{Name: tc.testName, Classname: "example.org/project", Status: tc.status, Type: tc.kind, Message: tc.message}
			got := mineIdentity("go", "unknown", "example.org/project", raw)
			if tc.synthetic {
				if got.Identity.ID != "" || got.InfraReason != "synthetic_go_error_record" || got.IdentityError != "synthetic_go_error_record" {
					t.Fatal("recognized synthetic error promoted as a test identity")
				}
			} else if got.Identity.ID == "" || got.InfraReason != "" {
				t.Fatal("near-miss ordinary record reclassified as infrastructure")
			}
			if !reflect.DeepEqual(got.Case, raw) {
				t.Fatal("synthetic mapping changed raw case")
			}
		})
	}
}
