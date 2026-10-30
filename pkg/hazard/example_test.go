package hazard_test

import (
	"context"
	"fmt"
	"log"

	"github.com/omorsi45/hazard/pkg/hazard"
)

// Select returns either a narrowed list of tests or the directive to run the
// whole suite. Stop on an error: the Decision is then not a selection.
func ExampleSelect() {
	d, err := hazard.Select(context.Background(), hazard.Options{RepoDir: "."})
	if err != nil {
		log.Fatal(err)
	}
	if d.FullSuite {
		fmt.Println("run the complete suite:", d.AbstainReason)
		return
	}
	for _, t := range d.Tests {
		fmt.Println(t.ID, t.Source)
	}
}
