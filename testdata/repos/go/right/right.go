// Package right is the right arm of the fixture dependency diamond.
package right

import "example.com/hazardfixture/core"

// Value returns the right arm's value.
func Value() int { return core.Base() + 2 }
