package report

import (
	"os"
	"strings"
	"testing"
)

func TestSharedFixtureItems(t *testing.T) {
	data, err := os.ReadFile("../testdata/shared.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"items": 3`) || Items != 3 {
		t.Fatalf("shared.json does not record 3 items:\n%s", data)
	}
}
