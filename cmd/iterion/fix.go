package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var fixOpts cli.FixOptions

// fixCmd applies the mechanical remedies of compile diagnostics — the edits
// a diagnostic carries because its fix is the same every time — to .bot
// files, on their own bytes, proving each result before it is written.
var fixCmd = &cobra.Command{
	Use:   "fix <file.bot | bundle dir | dir>...",
	Short: "Apply the mechanical remedies of compile diagnostics to .bot files, provably",
	Long: "Apply, to each .bot file, the remedies that are the same every time — today\n" +
		"C137, a {{ref}} an author quoted in a tool's command: or postcondition: (the\n" +
		"runtime shell-quotes a ref already; the two quotings cancel) loses exactly the\n" +
		"quotes around it. The edit is made on the file's own bytes — comments and\n" +
		"layout untouched — and proven before it is written: the text parses, and\n" +
		"compiles to the same diagnostics minus the fixed. Quotes that hold more than\n" +
		"the reference are not mechanical and are left to you, said so; so is every\n" +
		"other diagnostic. --dry-run lists every edit and writes nothing.",
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fixOpts.Paths = args
		fixOpts.Printer = newPrinter()
		_, err := cli.RunFix(fixOpts)
		return err
	},
}

func init() {
	fixCmd.Flags().BoolVar(&fixOpts.DryRun, "dry-run", false, "List every edit and write nothing")
	rootCmd.AddCommand(fixCmd)
}
