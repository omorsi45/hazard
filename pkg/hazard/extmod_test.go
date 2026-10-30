package hazard_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExternalModuleCanImportThePublicPackage builds a separate module that
// requires github.com/omorsi45/hazard and calls hazard.Select. Outside the
// repository the internal packages are out of reach, so this fails unless the
// public package is complete on its own.
func TestExternalModuleCanImportThePublicPackage(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("testdata", "extmod")
	work := t.TempDir()

	goMod, err := os.ReadFile(filepath.Join(src, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	const relative = "=> ../../../.."
	if !bytes.Contains(goMod, []byte(relative)) {
		t.Fatalf("testdata go.mod must replace the module with %q: %s", relative, goMod)
	}
	// The replace path points at this checkout; it contains spaces on some
	// machines, so it is written as a quoted string.
	absolute := `=> "` + filepath.ToSlash(repo) + `"`
	files := map[string][]byte{"go.mod": bytes.Replace(goMod, []byte(relative), []byte(absolute), 1)}
	for _, name := range []string{"main.go"} {
		if files[name], err = os.ReadFile(filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	if files["go.sum"], err = os.ReadFile(filepath.Join(repo, "go.sum")); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(work, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("go", "build", "-o", filepath.Join(work, "extmod.bin"), "./...")
	cmd.Dir = work
	cmd.Env = os.Environ()
	// The child must use the same toolchain as this test; take it from the
	// environment instead of naming a version here.
	if tc := os.Getenv("GOTOOLCHAIN"); tc != "" {
		cmd.Env = append(cmd.Env, "GOTOOLCHAIN="+tc)
	}
	// The external module requires only hazard. The requirements of hazard's own
	// dependencies are missing from its go.mod, and the default -mod=readonly
	// refuses to add them ("updates to go.mod needed"). -mod=mod lets this
	// throwaway copy add them; nothing else about the build is relaxed.
	cmd.Env = append(cmd.Env, "GOFLAGS="+strings.TrimSpace(os.Getenv("GOFLAGS")+" -mod=mod"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build in the external module failed: %v\n%s", err, strings.TrimSpace(out.String()))
	}
}
