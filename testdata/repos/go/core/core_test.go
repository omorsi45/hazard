package core

import "testing"

func TestFoo(t *testing.T) {
	if got := Add(1, 2); got != 3 {
		t.Fatalf("Add(1, 2) = %d, want 3", got)
	}
}

func TestFooBar(t *testing.T) {
	if got := Base(); got != 10 {
		t.Fatalf("Base() = %d, want 10", got)
	}
}

func TestAddSubtestsWithSpaces(t *testing.T) {
	for _, tc := range []struct {
		name       string
		a, b, want int
	}{
		{"adds small numbers", 1, 2, 3},
		{"adds with a zero operand", 0, 5, 5},
		{"adds two negative numbers", -1, -2, -3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Add(tc.a, tc.b); got != tc.want {
				t.Fatalf("Add(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestLongNameWithoutRegexpMetacharactersToExerciseSelectionPatternLength(t *testing.T) {
	if got := Add(Base(), Base()); got != 20 {
		t.Fatalf("Add(Base(), Base()) = %d, want 20", got)
	}
}
