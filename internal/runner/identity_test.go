package runner

import (
	"errors"
	"reflect"
	"testing"
)

func TestGoIdentity(t *testing.T) {
	for _, tc := range []struct{ hint, name, want string }{
		{"example.com/project/pkg", "TestParent/Sub Test", "go:example.com/project/pkg:TestParent/Sub_Test"},
		{"fixture", "TestRoot", "go:fixture:TestRoot"},
		{"net/http", "TestA\tB", "go:net/http:TestA_B"},
	} {
		c := JUnitCase{Name: tc.name, Suite: "short-suite", File: "pkg_test.go"}
		got, err := NormalizeJUnitCase("go", tc.hint, c)
		if err != nil || got.ID != tc.want || got.Package != tc.hint || got.Language != "go" || got.File != c.File || got.RunnerID != "" || got.Unstable {
			t.Fatalf("verified Go identity: %+v %v", got, err)
		}
		again, err := NormalizeJUnitCase("go", tc.hint, c)
		if err != nil || !reflect.DeepEqual(got, again) || c.Name != tc.name {
			t.Fatalf("identity is not stable or raw name changed: %+v %+v %v", got, again, err)
		}
	}
	for _, tc := range []struct{ hint, suite, classname string }{
		{"", "short-suite", "short-class"},
		{"", "example.com/project/pkg", "example.com/project/pkg"},
		{"one two", "", ""},
		{"./...", "", ""},
		{"example.com/a:example.com/b", "", ""},
	} {
		got, err := NormalizeJUnitCase("go", tc.hint, JUnitCase{Name: "TestA", Suite: tc.suite, Classname: tc.classname})
		var identityErr *IdentityError
		if !errors.As(err, &identityErr) || got.ID != "" {
			t.Fatalf("ambiguous package invented identity: %+v %v", got, err)
		}
	}
}

func TestPythonIdentity(t *testing.T) {
	for _, tc := range []struct{ name, classname, hint, file, want string }{
		{"test_value[a b, c:d]", "tests.test_values.TestWidget", "tests.test_values", "tests/test_values.py", "py:tests.test_values.TestWidget:test_value[a b, c:d]"},
		{"test_value[a  b]", "tests.test_values.TestWidget", "", "", "py:tests.test_values.TestWidget:test_value[a  b]"},
		{"test_value[a-b/[]]", "test_values", "", "", "py:test_values:test_value[a-b/[]]"},
		{"test_value", "", "tests.test_values", "tests/test_values.py", "py:tests.test_values:test_value"},
	} {
		c := JUnitCase{Name: tc.name, Classname: tc.classname, File: tc.file}
		got, err := NormalizeJUnitCase("python", tc.hint, c)
		if err != nil || got.ID != tc.want || got.Name != tc.name || got.File != tc.file || got.Language != "python" || got.RunnerID != "" || got.Unstable {
			t.Fatalf("Python parametrization or runnable uncertainty lost: %+v %v", got, err)
		}
		if tc.classname != "" && got.Package != tc.classname {
			t.Fatalf("classname rewritten without verified mapping: %+v", got)
		}
	}
	for _, c := range []JUnitCase{
		{Name: "test_value", File: "tests/test_values.py"},
		{Name: "test_value", Classname: "   "},
		{Name: "test_value", Classname: "tests.test_values:ambiguous"},
	} {
		got, err := NormalizeJUnitCase("python", "", c)
		var identityErr *IdentityError
		if !errors.As(err, &identityErr) || got.ID != "" {
			t.Fatalf("unverified module invented identity: %+v %v", got, err)
		}
	}
}

func TestUnstableIdentity(t *testing.T) {
	for _, tc := range []struct {
		lang, name string
		unstable   bool
	}{
		{"python", "test_value[<Widget object at unknown>]", true},
		{"python", "test_value[<Widget object at 0xABC123>]", true},
		{"python", "test_value[0xabc]", true},
		{"python", "test_value[ordinary]", false},
		{"go", "TestParent/0xabc", true},
	} {
		t.Run(tc.lang+"/"+tc.name, func(t *testing.T) {
			c := JUnitCase{Name: tc.name, Classname: "tests.test_values", Properties: map[string]string{"raw": "value"}}
			got, err := NormalizeJUnitCase(tc.lang, "example.com/project/pkg", c)
			if err != nil || got.ID == "" || got.Unstable != tc.unstable || (got.IdentityReason != "") != tc.unstable {
				t.Fatalf("unstable identity was lost or removed: %+v %v", got, err)
			}
			if c.Name != tc.name || c.Classname != "tests.test_values" || c.Properties["raw"] != "value" {
				t.Fatalf("raw case changed: %+v", c)
			}
		})
	}
	for _, tc := range []struct{ lang, name string }{
		{"", "test_value"}, {"ruby", "test_value"}, {"python", ""}, {"go", "  "}, {"python", "test\x00value"},
	} {
		t.Run("invalid/"+tc.lang+"/"+tc.name, func(t *testing.T) {
			got, err := NormalizeJUnitCase(tc.lang, "example.com/project/pkg", JUnitCase{Name: tc.name})
			var identityErr *IdentityError
			if !errors.As(err, &identityErr) || got.ID != "" || identityErr.Reason == "" {
				t.Fatalf("malformed identity did not return a typed error: %+v %v", got, err)
			}
		})
	}
}
