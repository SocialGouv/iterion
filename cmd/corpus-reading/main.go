// Command corpus-reading checks, or rewrites, the committed corpus reading
// internal/mdcode's witness pins. `task corpus:reading -- --write` rewrites it
// for a change that edits a pinned file; release-it runs it with -cut from its
// before:git:beforeRelease hook, after release-floors, so the release commit
// re-pins the CHANGELOG.md and chart edits it ships and refuses — aborting the
// cut — any drift it did not make. A check over a stale golden exits 1 naming
// every moved path.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/internal/corpusreading"
)

func main() {
	var opts corpusreading.Options
	flag.StringVar(&opts.Root, "root", ".", "repository root")
	flag.BoolVar(&opts.Write, "write", false, "rewrite the golden over the whole corpus (a reviewed change's gesture); without -write or -cut, check only")
	flag.BoolVar(&opts.Cut, "cut", false, "rewrite only the pins of the files the uncommitted release cut edited, refusing any other drift (release-it's before:git:beforeRelease hook)")
	flag.Parse()
	opts.Stdout = os.Stdout
	if err := corpusreading.Run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "corpus-reading: %v\n", err)
		os.Exit(1)
	}
}
