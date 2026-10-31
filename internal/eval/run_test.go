package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteProducesTheFourFilesOfASyntheticRun(t *testing.T) {
	rep, built := sampleReport(t)
	dir := filepath.Join(t.TempDir(), "report", "nested")
	if err := (&Result{Report: rep, Pairs: built.Pairs}).Write(dir); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return string(data)
	}
	var doc struct {
		Schema int `json:"schema_version"`
		Files  Files
	}
	if err := json.Unmarshal([]byte(read(FileReport)), &doc); err != nil || doc.Schema != SchemaVersion || doc.Files.Pairs != FilePairs {
		t.Errorf("report.json: %v %+v", err, doc)
	}
	if page := read(FileHTML); !strings.Contains(page, "<title>Hazard eval: "+SyntheticLabel) {
		t.Errorf("report.html has the wrong title:\n%.200s", page)
	}
	if pairs := read(FilePairs); !strings.HasPrefix(pairs, "# "+SyntheticLabel+"\nevent,test,p,label,config\n") || strings.Count(pairs, "\n") != 2+len(built.Pairs) {
		t.Errorf("pairs.csv = %.120q", pairs)
	}
	if rel := read(FileReliability); !strings.HasPrefix(rel, "# "+SyntheticLabel+"\nconfig,bin,lo,hi,count,mean_p,observed_rate\n") {
		t.Errorf("reliability.csv = %.120q", rel)
	}
}

// A run without the mutation population has no pairs and no bins: those files
// are not written, and the report does not name them.
func TestWriteOfARealOnlyRunWritesNoPairsOrBins(t *testing.T) {
	rep, _ := sampleReport(t)
	rep.Synthetic = nil
	rep.Files = Files{Report: FileReport, HTML: FileHTML}
	dir := t.TempDir()
	if err := (&Result{Report: rep}).Write(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "report.html,report.json" {
		t.Errorf("files = %s, want report.html and report.json only", got)
	}
	data, _ := os.ReadFile(filepath.Join(dir, FileReport))
	if strings.Contains(string(data), "pairs.csv") || strings.Contains(string(data), "reliability.csv") {
		t.Error("report.json names a file that was not written")
	}
}
