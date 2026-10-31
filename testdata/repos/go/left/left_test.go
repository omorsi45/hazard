package left

import "testing"

func TestValue(t *testing.T) {
	if got := Value(); got != 11 {
		t.Fatalf("Value() = %d, want 11", got)
	}
}
