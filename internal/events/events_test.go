package events

import (
	"fmt"
	"testing"
)

// TestPartitionPinnedValues pins the assignment to buckets computed outside
// this package (SHA-256 of the group, first eight bytes read big-endian,
// modulo 100), including both bucket boundaries.
func TestPartitionPinnedValues(t *testing.T) {
	cases := []struct {
		group, want string
		bucket      int
	}{
		{"mut:a/x.go", PartitionTrain, 59}, // last bucket of train
		{"group-3", PartitionTrain, 48},
		{"group-4", PartitionTrain, 4},
		{"ci:0123456789abcdef", PartitionTrain, 27},
		{"group-10", PartitionTune, 63},
		{"group-5", PartitionTune, 78},
		{"group-8", PartitionTune, 79}, // last bucket of tune
		{"group-1", PartitionTest, 82},
		{"group-7", PartitionTest, 95},
		{"mut:internal/engine/engine.go", PartitionTest, 86},
	}
	for _, tc := range cases {
		if got := Partition(tc.group); got != tc.want {
			t.Errorf("Partition(%q) = %q, want %q (bucket %d)", tc.group, got, tc.want, tc.bucket)
		}
	}
}

func TestPartitionIsDeterministicAndCoversAllThreeValues(t *testing.T) {
	const n = 20000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		group := fmt.Sprintf("ci:%040x", i*7919)
		first := Partition(group)
		if again := Partition(group); again != first {
			t.Fatalf("Partition(%q) = %q then %q", group, first, again)
		}
		counts[first]++
	}
	if len(counts) != 3 {
		t.Fatalf("Partition produced %v, want exactly train, tune and test", counts)
	}
	for name, want := range map[string]float64{PartitionTrain: 0.60, PartitionTune: 0.20, PartitionTest: 0.20} {
		share := float64(counts[name]) / n
		if share < want-0.02 || share > want+0.02 {
			t.Errorf("share of %s = %.4f, want %.2f +- 0.02 (counts %v)", name, share, want, counts)
		}
	}
}
