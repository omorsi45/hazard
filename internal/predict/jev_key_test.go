package predict

import (
	"fmt"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
)

func TestJevAPIKeyIsNotPrintable(t *testing.T) {
	opts := JevOptions{Config: jevConfig(), Privacy: config.Defaults().Privacy, RubricVersion: "r1", APIKey: testKey}
	j := newTestJev(t, nil)
	values := map[string]any{"options": opts, "options pointer": &opts, "client": j}
	for name, v := range values {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			if out := fmt.Sprintf(format, v); strings.Contains(out, testKey) {
				t.Fatalf("%s printed with %s leaks the key: %s", name, format, out)
			}
		}
	}
	if out := fmt.Sprint(opts); !strings.Contains(out, "typesafe/jev-1.13") {
		t.Fatalf("the printed options should still name the model: %s", out)
	}
}
