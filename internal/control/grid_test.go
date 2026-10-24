package control

import (
	"math"
	"testing"
)

func TestParseGridValid(t *testing.T) {
	tests := []struct {
		in   string
		want []float64
	}{
		{"0.1:0.3:0.1", []float64{0.1, 0.2, 0.3}}, // 0.1+2*0.1 is 0.30000000000000004: end is inclusive
		{"0.2:0.2:0.1", []float64{0.2}},
		{"0.01:0.05:0.02", []float64{0.01, 0.03, 0.05}},
		{"0.01:0.04:0.02", []float64{0.01, 0.03}},
		{"0.1:0.29999:0.1", []float64{0.1, 0.2}},
		// values are rounded to 1e-9
		{"0.1:0.2:0.0333333333333", []float64{0.1, 0.133333333, 0.166666667, 0.2}},
	}
	for _, tc := range tests {
		got, err := ParseGrid(tc.in)
		if err != nil {
			t.Errorf("ParseGrid(%q) error: %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("ParseGrid(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ParseGrid(%q)[%d] = %.17g, want %.17g", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseGridStandardCalibrationGrid(t *testing.T) {
	got, err := ParseGrid("0.005:0.5:0.005")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100", len(got))
	}
	for i, v := range got {
		want := math.Round(0.005*float64(i+1)*1e9) / 1e9
		if v != want {
			t.Fatalf("grid[%d] = %.17g, want %.17g", i, v, want)
		}
		if i > 0 && !(v > got[i-1]) {
			t.Fatalf("grid not strictly ascending at %d: %v then %v", i, got[i-1], v)
		}
	}
	if got[0] != 0.005 || got[99] != 0.5 {
		t.Errorf("endpoints = %v, %v; want 0.005 and 0.5 (end inclusive)", got[0], got[99])
	}
}

func TestParseGridPointLimit(t *testing.T) {
	got, err := ParseGrid("0.00005:0.5:0.00005")
	if err != nil {
		t.Fatalf("exactly 10000 points must be accepted: %v", err)
	}
	if len(got) != 10000 {
		t.Errorf("len = %d, want 10000", len(got))
	}
	if _, err := ParseGrid("0.00005:0.50005:0.00005"); err == nil {
		t.Error("10001 points must be rejected")
	}
	// A pathological step must be rejected before anything is allocated.
	if _, err := ParseGrid("0.1:0.9:1e-300"); err == nil {
		t.Error("1e-300 step must be rejected")
	}
}

func TestParseGridRejects(t *testing.T) {
	bad := []string{
		"", "0.1", "0.1:0.2", "0.1:0.2:0.1:0.1", "::",
		"a:0.2:0.1", "0.1:b:0.1", "0.1:0.2:c",
		" 0.1:0.2:0.1", "0.1:0.2:0.1 ",
		"NaN:0.2:0.1", "0.1:NaN:0.1", "0.1:0.2:NaN",
		"Inf:0.2:0.1", "0.1:Inf:0.1", "0.1:0.2:Inf", "-Inf:0.2:0.1",
		"0:0.5:0.1",                       // start must be > 0
		"-0.1:0.5:0.1",                    // negative start
		"0.5:0.1:0.1",                     // start > end
		"0.1:1:0.1",                       // end must be < 1
		"0.1:1.5:0.1",                     // end above 1
		"0.1:0.2:0",                       // step must be > 0
		"0.1:0.2:-0.1",                    // negative step
		"1e-10:0.5:0.1",                   // rounds to 0 at 1e-9
		"0.1:0.1000000005:1e-10",          // collapses to duplicates after rounding
		"0.9999999996:0.9999999999:1e-10", // start rounds to 1 at 1e-9
	}
	for _, in := range bad {
		if got, err := ParseGrid(in); err == nil {
			t.Errorf("ParseGrid(%q) = %v, want error", in, got)
		}
	}
}
