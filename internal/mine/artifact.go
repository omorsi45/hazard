package mine

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/omorsi45/hazard/internal/runner"
)

type ArtifactEntry struct {
	Name   string
	SHA256 string
	Cases  []runner.JUnitCase
}

const (
	maxArtifactInputBytes    = 64 << 20
	maxArtifactExpandedBytes = 128 << 20
	maxArtifactReportBytes   = 32 << 20
	maxArtifactEntries       = 1024
	maxArtifactCases         = 100000
	maxArtifactProperties    = 1000000
)

type artifactLimits struct {
	inputBytes, expandedBytes, reportBytes int64
	entries, cases, properties             int
}

// DecodeArtifact reads ZIP, gzip or XML without extracting files. An empty hint
// detects the format. An empty glob defaults to **/junit*.xml. Matching is
// case-sensitive with forward-slash paths: ** is a whole segment matching zero
// or more segments; every other segment uses path.Match (including its escapes).
// Plain XML and gzip have the logical name junit.xml. SHA256 identifies decoded
// report bytes, independently of the raw artifact digest stored by orchestration.
func DecodeArtifact(r io.Reader, formatHint, glob string) ([]ArtifactEntry, error) {
	return decodeArtifact(r, formatHint, glob, artifactLimits{
		inputBytes: maxArtifactInputBytes, expandedBytes: maxArtifactExpandedBytes,
		reportBytes: maxArtifactReportBytes, entries: maxArtifactEntries,
		cases: maxArtifactCases, properties: maxArtifactProperties,
	})
}

func decodeArtifact(r io.Reader, formatHint, glob string, limits artifactLimits) ([]ArtifactEntry, error) {
	if glob == "" {
		glob = "**/junit*.xml"
	}
	if !artifactValidPath(glob) || strings.HasPrefix(glob, `\`) {
		return nil, fmt.Errorf("invalid artifact glob path")
	}
	pattern := strings.Split(glob, "/")
	for _, segment := range pattern {
		if _, err := path.Match(segment, ""); err != nil {
			return nil, fmt.Errorf("invalid artifact glob")
		}
	}
	raw, err := readArtifactBytes(r, limits.inputBytes, "input")
	if err != nil {
		return nil, err
	}
	if formatHint == "" {
		switch {
		case bytes.HasPrefix(raw, []byte("PK")):
			formatHint = "zip"
		case bytes.HasPrefix(raw, []byte{0x1f, 0x8b}):
			formatHint = "gzip"
		default:
			formatHint = "xml"
		}
	}
	var entries []ArtifactEntry
	caseCount, propertyCount := 0, 0
	appendReport := func(name string, data []byte) error {
		if int64(len(data)) > limits.reportBytes {
			return fmt.Errorf("artifact report exceeds byte limit")
		}
		cases, err := runner.ParseJUnit(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("invalid artifact JUnit report")
		}
		if len(cases) > limits.cases-caseCount {
			return fmt.Errorf("artifact exceeds aggregate cases limit")
		}
		for _, c := range cases {
			if len(c.Properties) > limits.properties-propertyCount {
				return fmt.Errorf("artifact exceeds aggregate properties limit")
			}
			propertyCount += len(c.Properties)
		}
		caseCount += len(cases)
		if !artifactMatch(pattern, name) {
			return nil
		}
		entries = append(entries, ArtifactEntry{Name: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Cases: cases})
		return nil
	}
	switch formatHint {
	case "xml":
		err = appendReport("junit.xml", raw)
	case "gzip":
		reader, openErr := gzip.NewReader(bytes.NewReader(raw))
		if openErr != nil {
			return nil, fmt.Errorf("invalid gzip artifact")
		}
		data, readErr := readArtifactBytes(reader, limits.reportBytes, "report")
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("invalid gzip artifact")
		}
		err = appendReport("junit.xml", data)
	case "zip":
		archive, openErr := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if openErr != nil {
			return nil, fmt.Errorf("invalid ZIP artifact")
		}
		if len(archive.File) > limits.entries {
			return nil, fmt.Errorf("artifact exceeds ZIP entries limit")
		}
		seen := make(map[string]bool, len(archive.File))
		var declaredExpanded uint64
		for _, member := range archive.File {
			name := strings.TrimSuffix(member.Name, "/")
			if !artifactValidPath(name) || strings.Contains(member.Name, `\`) {
				return nil, fmt.Errorf("unsafe artifact member path")
			}
			if member.Mode().Type() & ^fs.ModeDir != 0 {
				return nil, fmt.Errorf("unsupported artifact member type")
			}
			if seen[name] {
				return nil, fmt.Errorf("duplicate artifact member name")
			}
			seen[name] = true
			if member.UncompressedSize64 > uint64(limits.expandedBytes)-declaredExpanded {
				return nil, fmt.Errorf("artifact exceeds declared expanded byte limit")
			}
			declaredExpanded += member.UncompressedSize64
			if !member.FileInfo().IsDir() && artifactMatch(pattern, member.Name) && member.UncompressedSize64 > uint64(limits.reportBytes) {
				return nil, fmt.Errorf("artifact report exceeds declared byte limit")
			}
		}
		var actualExpanded int64
		for _, member := range archive.File {
			if member.FileInfo().IsDir() || !artifactMatch(pattern, member.Name) {
				continue
			}
			reader, openErr := member.Open()
			if openErr != nil {
				return nil, fmt.Errorf("invalid ZIP report member")
			}
			limit, resource := limits.reportBytes, "report"
			if remaining := limits.expandedBytes - actualExpanded; remaining < limit {
				limit, resource = remaining, "expanded content"
			}
			data, readErr := readArtifactBytes(reader, limit, resource)
			closeErr := reader.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, fmt.Errorf("invalid ZIP report member")
			}
			actualExpanded += int64(len(data))
			if err = appendReport(member.Name, data); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unsupported artifact format")
	}
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func readArtifactBytes(r io.Reader, limit int64, resource string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("artifact %s exceeds byte limit", resource)
	}
	if err != nil {
		return nil, fmt.Errorf("invalid artifact %s data", resource)
	}
	return data, nil
}

func artifactValidPath(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	segments := strings.Split(value, "/")
	if strings.Contains(segments[0], ":") {
		return false
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func artifactMatch(pattern []string, name string) bool {
	segments := strings.Split(name, "/")
	matched := make([]bool, len(segments)+1)
	matched[0] = true
	for _, segment := range pattern {
		if segment == "**" {
			for i := 1; i < len(matched); i++ {
				matched[i] = matched[i] || matched[i-1]
			}
			continue
		}
		for i := len(segments); i > 0; i-- {
			ok, _ := path.Match(segment, segments[i-1])
			matched[i] = matched[i-1] && ok
		}
		matched[0] = false
	}
	return matched[len(segments)]
}
