package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/repomap"
	"github.com/spf13/cobra"
)

var mapOpts struct {
	root  string
	check bool
}

var mapCmd = &cobra.Command{
	Use:   "map",
	Short: "Generate and check the repository's discoverability commons",
	Long: `Map writes the small, committed indexes an agent reads instead of
grepping a tree it has never seen: one row per Go package (with the
interfaces it exposes), one per docs page and ADR (with its status), and
one per bot bundle and skill.

Everything is derived deterministically — Go's own parser, markdown
headings, the bundle manifest loader. No model call, no embedding.

The artifacts are committed under docs/references/ and held to the tree
by a Go test, so a stale index fails the same check that runs the unit
suite:

  iterion map gen             # rewrite the committed maps
  iterion map gen --check     # fail if they drift, write nothing`,
}

var mapGenCmd = &cobra.Command{
	Use:   "gen",
	Short: "Rewrite the committed maps (or, with --check, fail on drift)",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := mapOpts.root
		if root == "" {
			root = "."
		}
		p := newPrinter()
		if mapOpts.check {
			stale, err := repomap.Stale(root)
			if err != nil {
				return err
			}
			if len(stale) > 0 {
				return fmt.Errorf("stale generated maps: %v — run `task map:gen` and commit", stale)
			}
			p.Line("Maps are fresh.")
			return nil
		}
		written, err := repomap.Write(root)
		if err != nil {
			return err
		}
		if len(written) == 0 {
			p.Line("Maps already up to date.")
			return nil
		}
		for _, rel := range written {
			p.Line("wrote %s", rel)
		}
		return nil
	},
}

func init() {
	f := mapGenCmd.Flags()
	f.StringVar(&mapOpts.root, "root", "", "Repository root (default: the working directory)")
	f.BoolVar(&mapOpts.check, "check", false, "Fail when a committed map drifts from the tree; write nothing")

	mapCmd.AddCommand(mapGenCmd)
	rootCmd.AddCommand(mapCmd)
}
