package runner

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TestIdentity separates a history identifier from a collected runnable ID.
type TestIdentity struct {
	ID, Language, Package, Name, File, RunnerID string
	Unstable                                    bool
	IdentityReason                              string
}

// IdentityError means report metadata cannot establish a history identity.
type IdentityError struct{ Reason string }

func (e *IdentityError) Error() string { return "JUnit identity: " + e.Reason }

// NormalizeJUnitCase accepts package hints already verified by its caller.
func NormalizeJUnitCase(lang, packageHint string, c JUnitCase) (TestIdentity, error) {
	if lang != "go" && lang != "python" {
		return TestIdentity{}, &IdentityError{Reason: "unsupported or missing language"}
	}
	if strings.TrimSpace(c.Name) == "" || !utf8.ValidString(c.Name) || strings.IndexFunc(c.Name, func(r rune) bool { return unicode.IsControl(r) && !unicode.IsSpace(r) }) >= 0 {
		return TestIdentity{}, &IdentityError{Reason: "missing or malformed test name"}
	}
	if lang == "python" {
		qualifier := c.Classname
		if qualifier == "" {
			qualifier = packageHint
		}
		if qualifier == "" || strings.ContainsAny(qualifier, " :\t\n\r") {
			return TestIdentity{}, &IdentityError{Reason: "missing or ambiguous Python qualifier"}
		}
		return markJUnitInstability(TestIdentity{ID: "py:" + qualifier + ":" + c.Name, Language: lang, Package: qualifier, Name: c.Name, File: c.File}, c.Name), nil
	}
	if packageHint == "" || strings.ContainsAny(packageHint, " :\\\t\n\r") || strings.HasPrefix(packageHint, "/") || strings.HasSuffix(packageHint, "/") {
		return TestIdentity{}, &IdentityError{Reason: "missing or ambiguous verified Go package"}
	}
	for _, part := range strings.Split(packageHint, "/") {
		if part == "" || part == "." || part == ".." || part == "..." {
			return TestIdentity{}, &IdentityError{Reason: "invalid verified Go import path"}
		}
	}
	name := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return '_'
		}
		return r
	}, c.Name)
	return markJUnitInstability(TestIdentity{ID: "go:" + packageHint + ":" + name, Language: lang, Package: packageHint, Name: name, File: c.File}, c.Name), nil
}

var unstableJUnitIdentity = regexp.MustCompile(`(?i)\bobject at\b|0x[0-9a-f]+`)

func markJUnitInstability(identity TestIdentity, rawName string) TestIdentity {
	if unstableJUnitIdentity.MatchString(rawName + "\n" + identity.Package) {
		identity.Unstable = true
		identity.IdentityReason = "object representation or hexadecimal address in test identity"
	}
	return identity
}
