package mine

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"reflect"
	"strings"
	"testing"
)

const artifactXML = `<testsuite name="unit"><properties><property name="os" value="linux"/></properties><testcase name="works" classname="example/pkg" time="0.125"/><testcase name="broken"><failure message="assertion">details</failure></testcase></testsuite>`

func TestArtifactFormatsAndEntryIdentity(t *testing.T) {
	for _, format := range []string{"xml", "gzip", "zip"} {
		t.Run(format, func(t *testing.T) {
			raw := []byte(artifactXML)
			name := "junit.xml"
			switch format {
			case "gzip":
				raw = artifactGzip(t, raw)
			case "zip":
				name = "reports/unit/junit-results.xml"
				raw = artifactZIP(t, artifactMember{name: name, body: artifactXML})
			}
			for _, hint := range []string{format, ""} {
				entries, err := DecodeArtifact(bytes.NewReader(raw), hint, "")
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].Name != name {
					t.Fatalf("decoded entries = %#v, want one report named %q", entries, name)
				}
				entry := entries[0]
				wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(artifactXML)))
				if entry.SHA256 != wantHash {
					t.Fatalf("entry digest = %q, want decoded XML digest %q", entry.SHA256, wantHash)
				}
				if format != "xml" && entry.SHA256 == fmt.Sprintf("%x", sha256.Sum256(raw)) {
					t.Fatal("entry digest collapsed into raw archive digest")
				}
				if len(entry.Cases) != 2 || entry.Cases[0].Name != "works" || entry.Cases[0].Status != "pass" || entry.Cases[0].DurationMS == nil || *entry.Cases[0].DurationMS != 125 || entry.Cases[0].Properties["os"] != "linux" || entry.Cases[1].Status != "fail" || entry.Cases[1].Message != "assertion\ndetails" {
					t.Fatalf("parsed cases = %#v", entry.Cases)
				}
			}
		})
	}
}

func TestArtifactGlobSegments(t *testing.T) {
	raw := artifactZIP(t,
		artifactMember{name: "junit.xml", body: artifactXML},
		artifactMember{name: "a/junit-a.xml", body: artifactXML},
		artifactMember{name: "a/b/junit-b.xml", body: artifactXML},
		artifactMember{name: "a/TEST-unit.xml", body: artifactXML},
		artifactMember{name: "a/JUNIT.xml", body: artifactXML},
		artifactMember{name: "other.txt", body: "not XML"},
	)
	for _, tt := range []struct {
		glob string
		want []string
	}{
		{"", []string{"junit.xml", "a/junit-a.xml", "a/b/junit-b.xml"}},
		{"**/junit*.xml", []string{"junit.xml", "a/junit-a.xml", "a/b/junit-b.xml"}},
		{"*/junit?.xml", nil},
		{"a/junit-?.xml", []string{"a/junit-a.xml"}},
		{"a/**/junit*.xml", []string{"a/junit-a.xml", "a/b/junit-b.xml"}},
		{"**/TEST-*.xml", []string{"a/TEST-unit.xml"}},
		{"a/[J]UNIT.xml", []string{"a/JUNIT.xml"}},
		{"**/missing.xml", nil},
	} {
		t.Run(tt.glob, func(t *testing.T) {
			entries, err := DecodeArtifact(bytes.NewReader(raw), "zip", tt.glob)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("matched names = %v, want %v", names, tt.want)
			}
		})
	}
	for _, format := range []string{"xml", "gzip"} {
		raw := []byte(artifactXML)
		if format == "gzip" {
			raw = artifactGzip(t, raw)
		}
		entries, err := DecodeArtifact(bytes.NewReader(raw), format, "**/TEST-*.xml")
		if err != nil || len(entries) != 0 {
			t.Fatalf("unmatched %s = %#v, %v; want no entries", format, entries, err)
		}
	}
}

func TestArtifactLeadingGlobEscape(t *testing.T) {
	for _, tt := range []struct {
		format, name, glob string
	}{
		{"zip", "[report].xml", `\[report\].xml`},
		{"xml", "junit.xml", `\junit.xml`},
		{"gzip", "junit.xml", `\junit.xml`},
	} {
		t.Run(tt.format, func(t *testing.T) {
			matched, err := path.Match(tt.glob, tt.name)
			if err != nil || !matched {
				t.Fatalf("invalid path.Match regression fixture: matched=%v err=%v", matched, err)
			}
			raw := []byte(artifactXML)
			switch tt.format {
			case "zip":
				raw = artifactZIP(t, artifactMember{name: tt.name, body: artifactXML})
			case "gzip":
				raw = artifactGzip(t, raw)
			}
			entries, err := DecodeArtifact(bytes.NewReader(raw), tt.format, tt.glob)
			if err != nil || len(entries) != 1 || entries[0].Name != tt.name {
				t.Fatalf("valid leading glob escape rejected: entry count=%d err=%v", len(entries), err)
			}
		})
	}
}

func TestArtifactRejectsUnsafeMembers(t *testing.T) {
	for _, name := range []string{"../junit.xml", "/junit.xml", "C:/junit.xml", "C:junit.xml", `a\junit.xml`, "a//junit.xml", "a/./junit.xml", "a/../junit.xml", "", "a\x00/junit.xml", "a\n/junit.xml", "a/\xff.xml", "a//"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			body := artifactXML
			if strings.HasSuffix(name, "/") {
				body = ""
			}
			raw := artifactZIP(t, artifactMember{name: "junit.xml", body: artifactXML}, artifactMember{name: name, body: body})
			entries, err := DecodeArtifact(bytes.NewReader(raw), "zip", "**/junit*.xml")
			if err == nil || entries != nil {
				t.Fatalf("unsafe path %q accepted: entry count=%d err=%v", name, len(entries), err)
			}
		})
	}
	for _, mode := range []fs.FileMode{fs.ModeSymlink | 0o777, fs.ModeNamedPipe | 0o600, fs.ModeSocket | 0o600, fs.ModeDevice | 0o600, fs.ModeDevice | fs.ModeCharDevice | 0o600} {
		t.Run(mode.String(), func(t *testing.T) {
			raw := artifactZIP(t, artifactMember{name: "junit.xml", body: artifactXML}, artifactMember{name: "unmatched.bin", body: "target", mode: mode})
			entries, err := DecodeArtifact(bytes.NewReader(raw), "zip", "")
			if err == nil || entries != nil {
				t.Fatalf("unsupported type %v accepted: entry count=%d err=%v", mode, len(entries), err)
			}
		})
	}
}

func TestArtifactRejectsDuplicateIdentity(t *testing.T) {
	for _, members := range [][]artifactMember{
		{{name: "junit.xml", body: artifactXML}, {name: "junit.xml", body: artifactXML}},
		{{name: "unmatched.txt", body: "one"}, {name: "unmatched.txt", body: "two"}},
		{{name: "reports/", mode: fs.ModeDir | 0o755}, {name: "reports", body: "file"}},
	} {
		entries, err := DecodeArtifact(bytes.NewReader(artifactZIP(t, members...)), "zip", "")
		if err == nil || entries != nil {
			t.Fatalf("duplicate normalized identity accepted: entry count=%d err=%v", len(entries), err)
		}
	}
	raw := artifactZIP(t, artifactMember{name: "reports/", mode: fs.ModeDir | 0o755}, artifactMember{name: "reports/junit.xml", body: artifactXML})
	entries, err := DecodeArtifact(bytes.NewReader(raw), "zip", "")
	if err != nil || len(entries) != 1 || entries[0].Name != "reports/junit.xml" {
		t.Fatalf("safe directory rejected: entries=%#v err=%v", entries, err)
	}
}

func TestArtifactRejectsInvalidGlob(t *testing.T) {
	for _, glob := range []string{"/junit.xml", "../junit.xml", "a/../junit.xml", "a/./junit.xml", "a//junit.xml", "C:/junit.xml", "C:junit.xml", "[", "a/[a-", "a/", "a\x00/junit.xml", "a\n/junit.xml"} {
		t.Run(fmt.Sprintf("%q", glob), func(t *testing.T) {
			entries, err := DecodeArtifact(strings.NewReader(artifactXML), "xml", glob)
			if err == nil || entries != nil {
				t.Fatalf("invalid glob %q accepted: entries=%#v err=%v", glob, entries, err)
			}
		})
	}
	raw := artifactZIP(t, artifactMember{name: "junit[.xml", body: artifactXML})
	entries, err := DecodeArtifact(bytes.NewReader(raw), "zip", `**/junit\[.xml`)
	if err != nil || len(entries) != 1 {
		t.Fatalf("path.Match escape rejected: entries=%#v err=%v", entries, err)
	}
}

func TestArtifactMalformedNoPartialSuccess(t *testing.T) {
	badGzip := artifactGzip(t, []byte(artifactXML))
	badGzip[len(badGzip)-8] ^= 0xff
	for _, tt := range []struct {
		name, hint string
		raw        []byte
	}{
		{"broken zip", "zip", []byte("PKbroken")},
		{"broken gzip", "gzip", []byte{0x1f, 0x8b, 0}},
		{"gzip checksum", "gzip", badGzip},
		{"gzip trailing garbage", "gzip", append(artifactGzip(t, []byte(artifactXML)), []byte("garbage")...)},
		{"XML truncation", "xml", []byte(`<testsuite><testcase name="oops"/>`)},
		{"XML wrong root", "xml", []byte(`<html/>`)},
		{"unsupported hint", "tar", []byte(artifactXML)},
		{"late XML error", "zip", artifactZIP(t, artifactMember{name: "junit.xml", body: artifactXML}, artifactMember{name: "nested/junit.xml", body: `<testsuite>`})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := DecodeArtifact(bytes.NewReader(tt.raw), tt.hint, "")
			if err == nil || entries != nil {
				t.Fatalf("malformed artifact accepted partially: entries=%#v err=%v", entries, err)
			}
		})
	}
}

func TestArtifactErrorsAreBounded(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`<` + strings.Repeat("x", 1<<16) + `/>`),
		artifactZIP(t, artifactMember{name: "../" + strings.Repeat("x", 1<<15), body: "x"}),
	} {
		entries, err := DecodeArtifact(bytes.NewReader(raw), "", "")
		if err == nil || entries != nil || len(err.Error()) > 256 {
			t.Fatalf("unbounded error: entries=%#v err=%v", entries, err)
		}
	}
}

func TestArtifactInputLimit(t *testing.T) {
	limits := artifactTestLimits()
	limits.inputBytes = int64(len(artifactXML))
	entries, err := decodeArtifact(strings.NewReader(artifactXML), "xml", "", limits)
	if err != nil || len(entries) != 1 {
		t.Fatalf("exact input boundary rejected: entry count=%d err=%v", len(entries), err)
	}
	limits.inputBytes--
	r := &artifactCountingReader{Reader: strings.NewReader(artifactXML + strings.Repeat(" ", 1<<16))}
	entries, err = decodeArtifact(r, "xml", "", limits)
	if err == nil || entries != nil || !strings.Contains(err.Error(), "input") {
		t.Fatalf("oversized input accepted: entry count=%d err=%v", len(entries), err)
	}
	if r.read > limits.inputBytes+1 {
		t.Fatalf("input limit read %d bytes, want at most %d", r.read, limits.inputBytes+1)
	}
}

func TestArtifactReportByteLimit(t *testing.T) {
	for _, format := range []string{"xml", "gzip", "zip"} {
		t.Run(format, func(t *testing.T) {
			raw := []byte(artifactXML)
			switch format {
			case "gzip":
				raw = artifactGzip(t, raw)
			case "zip":
				raw = artifactZIP(t, artifactMember{name: "junit.xml", body: artifactXML})
			}
			limits := artifactTestLimits()
			limits.reportBytes = int64(len(artifactXML))
			entries, err := decodeArtifact(bytes.NewReader(raw), format, "", limits)
			if err != nil || len(entries) != 1 {
				t.Fatalf("exact report boundary rejected: entry count=%d err=%v", len(entries), err)
			}
			limits.reportBytes--
			entries, err = decodeArtifact(bytes.NewReader(raw), format, "", limits)
			if err == nil || entries != nil || !strings.Contains(err.Error(), "report") {
				t.Fatalf("oversized %s report accepted: entry count=%d err=%v", format, len(entries), err)
			}
		})
	}
}

func TestArtifactExpandedAggregateLimit(t *testing.T) {
	raw := artifactZIP(t,
		artifactMember{name: "junit.xml", body: artifactXML},
		artifactMember{name: "nested/junit.xml", body: artifactXML},
		artifactMember{name: "unmatched.txt", body: "ten bytes!"},
	)
	limits := artifactTestLimits()
	limits.expandedBytes = int64(2*len(artifactXML) + 10)
	entries, err := decodeArtifact(bytes.NewReader(raw), "zip", "", limits)
	if err != nil || len(entries) != 2 {
		t.Fatalf("exact expanded boundary rejected: entry count=%d err=%v", len(entries), err)
	}
	limits.expandedBytes--
	entries, err = decodeArtifact(bytes.NewReader(raw), "zip", "", limits)
	if err == nil || entries != nil || !strings.Contains(err.Error(), "expanded") {
		t.Fatalf("declared expanded total accepted: entry count=%d err=%v", len(entries), err)
	}
}

func TestArtifactEntryCountLimit(t *testing.T) {
	raw := artifactZIP(t, artifactMember{name: "junit.xml", body: artifactXML}, artifactMember{name: "unmatched.txt"})
	limits := artifactTestLimits()
	limits.entries = 2
	entries, err := decodeArtifact(bytes.NewReader(raw), "zip", "", limits)
	if err != nil || len(entries) != 1 {
		t.Fatalf("exact entry boundary rejected: entry count=%d err=%v", len(entries), err)
	}
	limits.entries = 1
	entries, err = decodeArtifact(bytes.NewReader(raw), "zip", "", limits)
	if err == nil || entries != nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("excessive ZIP entries accepted: entry count=%d err=%v", len(entries), err)
	}
}

func TestArtifactAggregateCasesAndProperties(t *testing.T) {
	raw := artifactZIP(t, artifactMember{name: "junit.xml", body: artifactXML}, artifactMember{name: "nested/junit.xml", body: artifactXML})
	for _, resource := range []string{"cases", "properties"} {
		t.Run(resource, func(t *testing.T) {
			limits := artifactTestLimits()
			if resource == "cases" {
				limits.cases = 4
			} else {
				limits.properties = 4
			}
			entries, err := decodeArtifact(bytes.NewReader(raw), "zip", "", limits)
			if err != nil || len(entries) != 2 {
				t.Fatalf("exact %s boundary rejected: entry count=%d err=%v", resource, len(entries), err)
			}
			if resource == "cases" {
				limits.cases--
			} else {
				limits.properties--
			}
			entries, err = decodeArtifact(bytes.NewReader(raw), "zip", "", limits)
			if err == nil || entries != nil || !strings.Contains(err.Error(), resource) {
				t.Fatalf("aggregate %s exceeded: entry count=%d err=%v", resource, len(entries), err)
			}
		})
	}
}

func TestArtifactActualReadLimit(t *testing.T) {
	r := &artifactCountingReader{Reader: strings.NewReader(artifactXML)}
	limit := int64(len(artifactXML) - 1)
	data, err := readArtifactBytes(r, limit, "report")
	if err == nil || data != nil {
		t.Fatalf("actual byte limit accepted %d bytes: err=%v", len(data), err)
	}
	if r.read > limit+1 {
		t.Fatalf("actual byte limit consumed %d bytes, want at most %d", r.read, limit+1)
	}
	data, err = readArtifactBytes(strings.NewReader(artifactXML), int64(len(artifactXML)), "report")
	if err != nil || string(data) != artifactXML {
		t.Fatalf("exact actual boundary rejected: len=%d err=%v", len(data), err)
	}
}

func TestArtifactDeclaredSizeOverflow(t *testing.T) {
	raw := artifactZIP(t, artifactMember{name: "junit.xml", body: artifactXML}, artifactMember{name: "unmatched.bin", body: "x"})
	// Hand-change the second central-directory record to a ZIP64 expanded size
	// whose addition to the first report would wrap uint64. It must fail before
	// opening either report, even though the impossible member is unmatched.
	first := bytes.Index(raw, []byte{'P', 'K', 1, 2})
	second := first + 46 + int(binary.LittleEndian.Uint16(raw[first+28:])) + int(binary.LittleEndian.Uint16(raw[first+30:])) + int(binary.LittleEndian.Uint16(raw[first+32:]))
	extraOffset := second + 46 + int(binary.LittleEndian.Uint16(raw[second+28:]))
	extra := []byte{1, 0, 8, 0, 255, 255, 255, 255, 255, 255, 255, 255}
	mutated := append([]byte(nil), raw[:extraOffset]...)
	mutated = append(mutated, extra...)
	mutated = append(mutated, raw[extraOffset:]...)
	binary.LittleEndian.PutUint32(mutated[second+24:], 0xffffffff)
	binary.LittleEndian.PutUint16(mutated[second+30:], uint16(len(extra)))
	end := bytes.LastIndex(mutated, []byte{'P', 'K', 5, 6})
	binary.LittleEndian.PutUint32(mutated[end+12:], binary.LittleEndian.Uint32(mutated[end+12:])+uint32(len(extra)))
	archive, err := zip.NewReader(bytes.NewReader(mutated), int64(len(mutated)))
	if err != nil || archive.File[1].UncompressedSize64 != ^uint64(0) {
		t.Fatalf("overflow fixture is not a readable ZIP64 directory: err=%v", err)
	}
	entries, err := DecodeArtifact(bytes.NewReader(mutated), "zip", "")
	if err == nil || entries != nil || !strings.Contains(err.Error(), "expanded") {
		t.Fatalf("overflowing declared total accepted: entry count=%d err=%v", len(entries), err)
	}
}

func TestArtifactZIPReadFailures(t *testing.T) {
	raw := artifactZIP(t, artifactMember{name: "junit.xml", body: artifactXML}, artifactMember{name: "nested/junit.xml", body: artifactXML})
	first := bytes.Index(raw, []byte{'P', 'K', 1, 2})
	second := first + 46 + int(binary.LittleEndian.Uint16(raw[first+28:])) + int(binary.LittleEndian.Uint16(raw[first+30:])) + int(binary.LittleEndian.Uint16(raw[first+32:]))
	for _, fault := range []string{"checksum", "compression", "expanded size"} {
		t.Run(fault, func(t *testing.T) {
			broken := append([]byte(nil), raw...)
			switch fault {
			case "checksum":
				broken[second+16] ^= 0xff
			case "compression":
				binary.LittleEndian.PutUint16(broken[second+10:], 99)
			case "expanded size":
				binary.LittleEndian.PutUint32(broken[second+24:], uint32(len(artifactXML)-1))
			}
			entries, err := DecodeArtifact(bytes.NewReader(broken), "zip", "")
			if err == nil || entries != nil {
				t.Fatalf("late %s failure accepted partially: entry count=%d err=%v", fault, len(entries), err)
			}
		})
	}
	// Unmatched members are metadata-checked but never inflated. A checksum
	// corrupted report is harmless to a glob selecting only the other report.
	raw[second+16] ^= 0xff
	entries, err := DecodeArtifact(bytes.NewReader(raw), "zip", "junit.xml")
	if err != nil || len(entries) != 1 || entries[0].Name != "junit.xml" {
		t.Fatalf("unmatched content was inflated: entry count=%d err=%v", len(entries), err)
	}
}

func TestArtifactInputErrorPrivacy(t *testing.T) {
	r := io.MultiReader(strings.NewReader(artifactXML), artifactErrorReader{})
	entries, err := DecodeArtifact(r, "xml", "")
	if err == nil || entries != nil || strings.Contains(err.Error(), "secret") || len(err.Error()) > 256 {
		t.Fatalf("input error leaked or returned partial results: entry count=%d err=%v", len(entries), err)
	}
}

func TestArtifactUnmatchedDirectReportValidation(t *testing.T) {
	for _, format := range []string{"xml", "gzip"} {
		for _, resource := range []string{"malformed XML", "cases", "properties"} {
			t.Run(format+"/"+resource, func(t *testing.T) {
				limits := artifactTestLimits()
				body := artifactXML
				switch resource {
				case "malformed XML":
					body = `<testsuite>`
				case "cases":
					limits.cases = 1
				case "properties":
					limits.properties = 1
				}
				raw := []byte(body)
				if format == "gzip" {
					raw = artifactGzip(t, raw)
				}
				entries, err := decodeArtifact(bytes.NewReader(raw), format, "**/TEST-*.xml", limits)
				if err == nil || entries != nil {
					t.Fatalf("unmatched %s bypassed %s validation: entry count=%d err=%v", format, resource, len(entries), err)
				}
			})
		}
	}
}

type artifactErrorReader struct{}

func (artifactErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("secret input diagnostic")
}

func artifactTestLimits() artifactLimits {
	return artifactLimits{inputBytes: 1 << 20, expandedBytes: 1 << 20, reportBytes: 1 << 20, entries: 1024, cases: 100000, properties: 1000000}
}

type artifactCountingReader struct {
	io.Reader
	read int64
}

func (r *artifactCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += int64(n)
	return n, err
}

type artifactMember struct {
	name, body string
	mode       fs.FileMode
}

func artifactZIP(t *testing.T, members ...artifactMember) []byte {
	t.Helper()
	var raw bytes.Buffer
	w := zip.NewWriter(&raw)
	for _, member := range members {
		h := &zip.FileHeader{Name: member.name, Method: zip.Deflate}
		if member.mode != 0 {
			h.SetMode(member.mode)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(member.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func artifactGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	w := gzip.NewWriter(&raw)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}
