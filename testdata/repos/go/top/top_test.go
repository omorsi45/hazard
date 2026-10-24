package top

import "testing"

func TestTotal(t *testing.T) {
	if got := Total(); got != 26 {
		t.Fatalf("Total() = %d, want 26", got)
	}
}
