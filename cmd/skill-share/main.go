// Command skill-share writes, checks, or re-pins the shared skills of the bot
// catalogue. `task skills:sync` writes every declared mirror from its source
// and runs under `task gen`; `task skills:bless` re-pins the declared copies
// and does NOT, so an edit to a variant — which no generator rewrites — lands
// as a reviewed line instead of being absorbed. A check over a tree that
// disagrees with internal/skillshare/families.go exits 1 naming every copy.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/internal/skillshare"
)

func main() {
	var opts skillshare.Options
	flag.StringVar(&opts.Root, "root", ".", "repository root")
	flag.BoolVar(&opts.Sync, "sync", false, "write every declared mirror from its family's source")
	flag.BoolVar(&opts.Bless, "bless", false, "write the mirrors, then re-pin every declared copy (a reviewed gesture, never an everyday generator)")
	flag.Parse()
	opts.Stdout = os.Stdout
	if err := skillshare.Run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "skill-share: %v\n", err)
		os.Exit(1)
	}
}
