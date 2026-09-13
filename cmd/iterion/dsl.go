package main

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/cli"
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

var dslMigrateOpts cli.MigrateDSLOptions

// dslMigrateCmd moves `.bot` files to a newer syntax profile (ADR-098) by
// the few edits that change meaning between profiles, proves the result
// reads as the same program, and raises the engine floor of the bundle
// manifests beside them.
var dslMigrateCmd = &cobra.Command{
	Use:   "migrate <file.bot | bundle dir | dir>...",
	Short: "Move .bot files to a newer syntax profile (dsl: 2), surgically and provably",
	Long: "Rewrite each .bot file for the target syntax profile by the edits that change\n" +
		"meaning between profiles and nothing else: the `dsl: 2` header goes in, the\n" +
		"profile-1 strict-escape directive comes out, every quoted literal holding a\n" +
		"backslash is re-spelled from its profile-1 value. Comments, blank lines, order,\n" +
		"a BOM and the line endings stay byte-identical.\n\n" +
		"Before anything is written the result is proven: both texts parse, they read\n" +
		"as the same document, and the file's catalogue identity (its `## ---`\n" +
		"frontmatter) is unchanged. Named prompts that keep paragraph breaks they used\n" +
		"to lose are reported (--show-prompts) and refused on --strict-prompts;\n" +
		"`project_root:` has no profile-2 form and is refused with the remedy.\n\n" +
		"A bundle's manifest gets `requires.iterion` raised to this build's version —\n" +
		"the one that reads the profile — so an older runner refuses it at admission.\n" +
		"--dry-run lists every change and writes nothing; --check writes nothing and\n" +
		"exits non-zero when a file would change.",
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dslMigrateOpts.Paths = args
		dslMigrateOpts.Printer = newPrinter()
		_, err := cli.MigrateDSL(dslMigrateOpts)
		return err
	},
}

func init() {
	dslSpecCmd.Flags().BoolVar(&dslSpecWrite, "write", false, "Regenerate every committed generated region in place instead of printing")
	dslSpecCmd.Flags().StringVar(&dslSpecRoot, "root", ".", "Repository root the --write paths are relative to")
	dslSpecCmd.Flags().StringVar(&dslSpecRegion, "region", "reference", "What to print: reference, skill, or 'table <kind>'")
	dslCmd.AddCommand(dslSpecCmd)
	dslMigrateCmd.Flags().IntVar(&dslMigrateOpts.To, "to", 2, "Target syntax profile")
	dslMigrateCmd.Flags().BoolVar(&dslMigrateOpts.DryRun, "dry-run", false, "List every change and write nothing")
	dslMigrateCmd.Flags().BoolVar(&dslMigrateOpts.Check, "check", false, "Write nothing; exit non-zero when a file would change")
	dslMigrateCmd.Flags().BoolVar(&dslMigrateOpts.StrictPrompts, "strict-prompts", false, "Refuse a file whose named prompts keep paragraph breaks they used to lose")
	dslMigrateCmd.Flags().BoolVar(&dslMigrateOpts.ShowPrompts, "show-prompts", false, "List the prompts whose rendering changes")
	dslMigrateCmd.Flags().StringVar(&dslMigrateOpts.Floor, "floor", "", "Engine version the migrated bundles must require at least (default: this build's)")
	dslCmd.AddCommand(dslMigrateCmd)
	rootCmd.AddCommand(dslCmd)
}
