package impact

import (
	"reflect"
	"testing"
)

func TestVectorLengthAndOrder(t *testing.T) {
	f := Features{
		Hops:              2,
		Reachable:         true,
		CoverageOverlap:   0.5,
		CoverageKnown:     true,
		CoverageStale:     true,
		CofailureRate:     0.25,
		CofailureSupport:  7,
		TestFailRate:      0.125,
		DaysSinceLastFail: 30,
		SharedSymbolHits:  3,
		SameFile:          true,
		SamePackage:       true,
		ChangedFileKinds:  [8]int{10, 11, 12, 13, 14, 15, 16, 17},
		LinesAdded:        100,
		LinesRemoved:      200,
		FilesChanged:      9,
		TestMedianMS:      450,
		TestIsIntegration: true,
	}
	want := []float64{
		2, 1, 0.5, 1, 1, 0.25, 7, 0.125, 30, 3, 1, 1,
		10, 11, 12, 13, 14, 15, 16, 17,
		100, 200, 9, 450, 1,
	}
	got := f.Vector()
	if len(got) != 25 {
		t.Fatalf("len(Vector()) = %d, want 25", len(got))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Vector() = %v, want %v", got, want)
	}
}

func TestVectorBoolsAreZeroOne(t *testing.T) {
	got := Features{Reachable: true}.Vector()
	want := make([]float64, 25)
	want[1] = 1 // Reachable; a reachable test at hop 0 keeps Hops 0
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Vector() = %v, want %v", got, want)
	}
}

func TestVectorUnreachableHopsIs99(t *testing.T) {
	got := Features{Reachable: false, Hops: 0}.Vector()
	if len(got) != 25 || got[0] != 99 || got[1] != 0 {
		t.Fatalf("unreachable vector head = %v, want hops 99 reachable 0", got)
	}
}

func TestFeatureVersion(t *testing.T) {
	if FeatureVersion != "f2" {
		t.Fatalf("FeatureVersion = %q, want f2", FeatureVersion)
	}
}
