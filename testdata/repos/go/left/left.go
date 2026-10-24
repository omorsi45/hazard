// Package left is the left arm of the fixture dependency diamond.
package left

import "example.com/hazardfixture/core"

// Value returns the left arm's value.
func Value() int { return core.Base() + 1 }
