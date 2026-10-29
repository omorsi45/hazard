package right

import "testing"

func TestValue(t *testing.T) {
	if got := Value(); got != 12 {
		t.Fatalf("Value() = %d, want 12", got)
	}
}
