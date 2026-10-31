package shared

import (
	"os"
	"strings"
	"testing"
)

func TestSharedFixtureName(t *testing.T) {
	data, err := os.ReadFile("../testdata/shared.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name": "`+Name+`"`) {
		t.Fatalf("shared.json does not name %q:\n%s", Name, data)
	}
}
