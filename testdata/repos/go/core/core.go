// Package core is the base of the fixture dependency diamond.
package core

// Base returns the value every other package builds on.
func Base() int { return 10 }

// Add returns the sum of a and b.
func Add(a, b int) int { return a + b }
