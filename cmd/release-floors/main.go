// Command release-floors realigns the syntax-floor pins to the release being
// cut: release-it runs it with --apply from its before:git:beforeRelease
// hook, `task release:floors` previews it. A refusal exits 1 and aborts the
// release before its commit.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/internal/floorsalign"
)

func main() {
	var opts floorsalign.Options
	flag.StringVar(&opts.Root, "root", ".", "repository root")
	flag.BoolVar(&opts.Apply, "apply", false, "write the realignments (release-it's before:git:beforeRelease hook); without it, preview only")
	flag.Parse()
	opts.Stdout = os.Stdout
	if err := floorsalign.Run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "release-floors: %v\n", err)
		os.Exit(1)
	}
}
