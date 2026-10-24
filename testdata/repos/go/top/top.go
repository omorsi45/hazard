// Package top is the apex of the fixture dependency diamond.
package top

import (
	"example.com/hazardfixture/gen"
	"example.com/hazardfixture/left"
	"example.com/hazardfixture/right"
)

// Total combines both arms with the generated schema version.
func Total() int { return left.Value() + right.Value() + gen.Version }
