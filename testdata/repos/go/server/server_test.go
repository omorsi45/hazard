package server

import (
	"os"
	"strings"
	"testing"
)

func TestDefaultPortMatchesConfig(t *testing.T) {
	data, err := os.ReadFile("../config/defaults.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "port: 8080") {
		t.Fatalf("defaults.yaml does not declare port 8080:\n%s", data)
	}
	if Port != 8080 {
		t.Fatalf("Port = %d, want 8080", Port)
	}
}
