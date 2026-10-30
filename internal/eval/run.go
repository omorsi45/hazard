package eval

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// Result is a finished evaluation: the report and the scored pairs behind its
// reliability tables.
type Result struct {
	Report Report
	Pairs  []Pair
}

// Write writes report.json, report.html and, when the report has a synthetic
// section, pairs.csv and reliability.csv into dir, creating it. Everything is
// rendered before the first file is written, so a rendering failure leaves
// the directory as it was.
func (r *Result) Write(dir string) error {
	files := map[string][]byte{}
	var err error
	if files[FileReport], err = r.Report.JSON(); err != nil {
		return err
	}
	if files[FileHTML], err = r.Report.HTML(); err != nil {
		return err
	}
	if s := r.Report.Synthetic; s != nil {
		var pairs, rel bytes.Buffer
		if err := WritePairs(&pairs, r.Pairs); err != nil {
			return fmt.Errorf("eval: render %s: %w", FilePairs, err)
		}
		if err := WriteReliability(&rel, s.Reliability); err != nil {
			return fmt.Errorf("eval: render %s: %w", FileReliability, err)
		}
		files[FilePairs], files[FileReliability] = pairs.Bytes(), rel.Bytes()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("eval: create the report directory: %w", err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return fmt.Errorf("eval: write %s: %w", name, err)
		}
	}
	return nil
}
