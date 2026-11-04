// Command fragment reads a build record's provenance and prints, for the
// RUN step on the Dockerfile's last line, the cache folders the console's
// hints place and the Dockerfile fragment they suggest. test/hints/
// managers.sh uses it to check the hints against real builds.
//
//	go run ./test/hints/fragment provenance.json
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/hoobiboobi/ci-accelerator/internal/hints"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fragment provenance.json")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err)
	}
	op, anc, name, err := history.LastRun(data)
	if err != nil {
		fail(err)
	}
	hs := hints.Analyze(op, anc)
	for _, h := range hs {
		for _, c := range h.Caches {
			fmt.Printf("PATH %s %s\n", h.Manager, orUnknown(c.Path))
		}
		for _, p := range h.Problems {
			fmt.Printf("PROBLEM %s %s %s %s\n", h.Manager, p.Kind, p.Mount, p.Setting)
		}
	}
	fmt.Println("FRAGMENT")
	fmt.Println(hints.Fragment(name, hs))
}

func orUnknown(p string) string {
	if p == "" {
		return "unknown"
	}
	return p
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fragment:", strings.TrimSpace(err.Error()))
	os.Exit(1)
}
