package eval

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCostModelImputesTheMedianOfTheKnownDurations(t *testing.T) {
	// Known: 10, 20, 40, 100. Even count: the mean of the middle pair, 20 and
	// 40, rounded up, is 30.
	c := NewCostModel(map[string]int{"a": 10, "b": 20, "c": 40, "d": 100})
	if c == nil {
		t.Fatal("NewCostModel returned nil for four measurements")
	}
	if got := c.Cost("c"); got != 40 {
		t.Errorf("Cost(c) = %v, want its measurement 40", got)
	}
	if got := c.Cost("x"); got != 30 {
		t.Errorf("Cost(x) = %v, want the median 30, never 0", got)
	}
	if got := c.Sum([]string{"a", "x", "y", "x"}); got != 10+30+30+30 {
		t.Errorf("Sum = %v, want 100 (a, then x, y, x each at the median)", got)
	}
	if c.Imputed() != 2 || c.Known() != 4 {
		t.Errorf("imputed=%d known=%d, want 2 distinct imputed tests and 4 known", c.Imputed(), c.Known())
	}
}

func TestCostModelOddCountMedianAndMeasuredZero(t *testing.T) {
	c := NewCostModel(map[string]int{"a": 0, "b": 20, "c": 40})
	if got := c.Cost("a"); got != 0 {
		t.Errorf("Cost(a) = %v: a measured 0 ms is a measurement", got)
	}
	if got := c.Cost("x"); got != 20 {
		t.Errorf("Cost(x) = %v, want the odd-count median 20", got)
	}
}

func TestCostModelWithoutMeasurementsIsNil(t *testing.T) {
	if NewCostModel(nil) != nil || NewCostModel(map[string]int{}) != nil {
		t.Error("NewCostModel with no measurement must be nil, not a model that prices everything at 0 or 1")
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadDurations(t *testing.T) {
	path := writeFile(t, "m.baseline.json", `{"base_sha":"abc123","tests":{"go:p:TestA":12,"go:p:TestB":-1,"go:p:TestC":0}}`)
	got, err := ReadDurations(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Durations{Path: path, BaseSHA: "abc123", MS: map[string]int{"go:p:TestA": 12, "go:p:TestC": 0}, Ignored: 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadDurations = %+v, want %+v", got, want)
	}
}

func TestReadDurationsRefusesWhatItCannotTrust(t *testing.T) {
	for name, content := range map[string]string{
		"not json":      `{"tests":`,
		"no tests key":  `{"base_sha":"abc"}`,
		"empty tests":   `{"base_sha":"abc","tests":{}}`,
		"wrong type":    `{"base_sha":"abc","tests":{"go:p:TestA":"fast"}}`,
		"trailing data": `{"base_sha":"abc","tests":{"go:p:TestA":1}} {}`,
	} {
		path := writeFile(t, "bad.json", content)
		if _, err := ReadDurations(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err = %v, want an error naming %s", name, err, path)
		}
	}
	if _, err := ReadDurations(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("a missing file must be an error")
	}
}
