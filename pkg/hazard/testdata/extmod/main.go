// Command extmod is built by a test as a separate module that depends on
// github.com/omorsi45/hazard: it proves the public package can be imported
// from outside the repository, where internal packages are out of reach.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/omorsi45/hazard/pkg/hazard"
)

func main() {
	d, err := hazard.Select(context.Background(), hazard.Options{RepoDir: "."})
	if err != nil {
		fmt.Fprintln(os.Stderr, "hazard:", err)
		os.Exit(2)
	}
	if d.FullSuite {
		fmt.Println("full suite:", d.AbstainReason)
		return
	}
	for _, t := range d.Tests {
		fmt.Println(t.ID)
	}
}
