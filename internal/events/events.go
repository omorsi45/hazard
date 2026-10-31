// Package events turns mutation records and CI regression labels into change
// events and replays them through the real selection engine, so that training
// and evaluation see the same L1 candidates and features as selection does.
// Every replay happens in a scratch clone: the repository being studied is
// never written.
package events

import (
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// Event origins.
const (
	OriginMutation = "mutation"
	OriginCIStrong = "ci_strong"
	OriginCIWeak   = "ci_weak"
)

// Partitions of the event space.
const (
	PartitionTrain = "train"
	PartitionTune  = "tune"
	PartitionTest  = "test"
)

// Event is one observed change and the tests it broke.
type Event struct {
	ID, Origin       string // Origin: "mutation" | "ci_strong" | "ci_weak"; ID: mutant ID or "ci:<commit sha>"
	Group            string // split group: mutation "mut:<file>", CI "ci:<commit sha>"
	BaseSHA, HeadSHA string
	Failing          []string  // F(c), sorted: confirmed killers, or the regressed tests of the label
	File, Operator   string    // mutation only
	Line             int       // mutation only
	Time             time.Time // committer time of BaseSHA (mutation) or of the commit (CI)
}

// Partition assigns a split group to "train", "tune" or "test" by a hash of
// the group, so the assignment is stable across runs and machines. With h the
// big-endian uint64 of the first eight bytes of sha256(group), modulo 100:
// train is h < 60, tune 60 <= h < 80 and test h >= 80.
func Partition(group string) string {
	sum := sha256.Sum256([]byte(group))
	switch h := binary.BigEndian.Uint64(sum[:8]) % 100; {
	case h < 60:
		return PartitionTrain
	case h < 80:
		return PartitionTune
	default:
		return PartitionTest
	}
}
