package mutate

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// safeEntryPath validates a tar entry name and returns it as a slash-separated
// relative path. Absolute names, drive letters, backslash separators that could
// escape, and ".." segments are rejected.
func safeEntryPath(name string) (string, error) {
	unsafe := func() (string, error) { return "", fmt.Errorf("mutate: unsafe path %q in the base archive", name) }
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || path.IsAbs(name) || filepath.VolumeName(name) != "" {
		return unsafe()
	}
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return unsafe()
		}
	}
	if first, _, _ := strings.Cut(name, "/"); len(first) >= 2 && first[1] == ':' {
		return unsafe()
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return unsafe()
	}
	return clean, nil
}

// extractTar unpacks a `git archive` tar stream into dst. Regular files and
// directories are written; the pax global header and symlinks are skipped (a
// missing symlink can only make the baseline fail, never hide a test). Any
// other entry type, and any entry that could land outside dst, is an error.
func extractTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mutate: read the base archive: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader, tar.TypeSymlink:
			continue
		case tar.TypeDir, tar.TypeReg:
		default:
			return fmt.Errorf("mutate: unsupported entry type %q for %q in the base archive", hdr.Typeflag, hdr.Name)
		}
		rel, err := safeEntryPath(hdr.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if inside, relErr := filepath.Rel(dst, target); relErr != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return fmt.Errorf("mutate: unsafe path %q in the base archive", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mutate: create %s: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mutate: create %s: %w", filepath.Dir(target), err)
		}
		mode := os.FileMode(0o644)
		if hdr.Mode&0o111 != 0 {
			mode = 0o755
		}
		if err := writeEntry(target, mode, tr); err != nil {
			return err
		}
	}
}

func writeEntry(target string, mode os.FileMode, r io.Reader) (err error) {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("mutate: create %s: %w", target, err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("mutate: close %s: %w", target, closeErr)
		}
	}()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("mutate: write %s: %w", target, err)
	}
	return nil
}
