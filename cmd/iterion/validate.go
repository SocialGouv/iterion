package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var validateOpts cli.ValidateOptions

var validateCmd = &cobra.Command{
	Use:   "validate <file.bot|file.botz|bundle-dir>",
	Short: "Parse, compile, and validate a workflow file",
	Long: `Parse, compile and validate a workflow file, a .botz bundle or a bundle directory.

With --exec, a program that compiles is then run twice under a dry run —
every condition true, then false — without a model, a shell or the
workspace: each node's prompts and commands are rendered and the references
left unresolved named, shell text is held to bash -n, humans and waits are
answered at once, and the report lists the nodes and edges no pass reached.
--fixtures answers the named nodes with recorded outputs instead of shapes.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunValidateWithContext(cmd.Context(), args[0], newPrinter(), validateOpts)
	},
}

func init() {
	validateCmd.Flags().BoolVar(&validateOpts.Exec, "exec", false, "after a clean compile, run the program under a dry run and report what it met")
	validateCmd.Flags().StringVar(&validateOpts.Fixtures, "fixtures", "", "JSON file of node outputs the dry run answers with ({node: output}, or a list of {node, output}); implies --exec")
	validateCmd.Flags().BoolVar(&validateOpts.Strict, "strict", false, "with --exec: exit non-zero when the dry run's report is not clean (a CI gate); implies --exec")
	rootCmd.AddCommand(validateCmd)
}
