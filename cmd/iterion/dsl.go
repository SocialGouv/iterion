package main

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
	"github.com/spf13/cobra"
)

// dslCmd groups the commands that inspect the .bot language itself rather
// than a workflow written in it.
var dslCmd = &cobra.Command{
	Use:   "dsl",
	Short: "Inspect the .bot DSL itself (property registry, generated reference)",
}

var (
	dslSpecWrite  bool
	dslSpecRoot   string
	dslSpecRegion string
)

// dslSpecCmd renders the property registry (pkg/dsl/spec) — the one source
// the grammar reference tables and the authoring skills' property section
// are generated from — or regenerates those committed documents in place.
// A conformance test holds the registry to the parser; `task dsl:check`
// fails when a committed rendering is stale.
var dslSpecCmd = &cobra.Command{
	Use:   "spec",
	Short: "Render the DSL property registry, or regenerate the committed docs from it",
	Long: "Render the property registry every kind of the .bot DSL is described by —\n" +
		"the full reference (default), the compact `skill` section, or the table of\n" +
		"one kind (`--region 'table agent'`) — or, with --write, regenerate every\n" +
		"committed generated region in place (docs/references/dsl-properties.md,\n" +
		"docs/references/dsl-grammar.md, SKILL.md, the whats-next DSL quickref).\n\n" +
		"The registry is held to the parser by a conformance test in both\n" +
		"directions, so what this prints is what the parser accepts.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if dslSpecWrite {
			changed, err := spec.Regenerate(dslSpecRoot)
			for _, f := range changed {
				fmt.Fprintf(os.Stderr, "wrote %s\n", f)
			}
			if err != nil {
				return err
			}
			if len(changed) == 0 {
				fmt.Fprintln(os.Stderr, "every generated region is already fresh")
			}
			return nil
		}
		body, err := spec.Render(dslSpecRegion)
		if err != nil {
			return err
		}
		_, err = os.Stdout.WriteString(body)
		return err
	},
}

func init() {
	dslSpecCmd.Flags().BoolVar(&dslSpecWrite, "write", false, "Regenerate every committed generated region in place instead of printing")
	dslSpecCmd.Flags().StringVar(&dslSpecRoot, "root", ".", "Repository root the --write paths are relative to")
	dslSpecCmd.Flags().StringVar(&dslSpecRegion, "region", "reference", "What to print: reference, skill, or 'table <kind>'")
	dslCmd.AddCommand(dslSpecCmd)
	rootCmd.AddCommand(dslCmd)
}
