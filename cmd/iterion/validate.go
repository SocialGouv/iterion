package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var validateOpts cli.ValidateOptions

var validateCmd = &cobra.Command{
	Use:   "validate <file.bot|file.bot.yaml|file.botz|bundle-dir>",
	Short: "Parse, compile, and validate a workflow file",
	Long: `Parse, compile and validate a workflow file, a .botz bundle, a bundle directory —
or an author document (x.bot.yaml, the YAML twin of a .bot), read into the program it
describes and validated as its .bot would be, every finding on the document.

With --exec, a program that compiles is then run twice under a dry run —
every condition true, then false — without a model, a shell or the
workspace: each node's prompts and commands are rendered and the references
left unresolved named, shell text is held to bash -n, humans and waits are
answered at once, and the report lists the nodes and edges no pass reached.
--fixtures answers the named nodes with recorded outputs instead of shapes.

With --connectors, every action: node is then resolved against the connector
catalogue installed on THIS machine (ADR-098): a connector no tier serves, or
an operation its package does not declare, is named with the reason. Opt-in,
because that answer is a property of the install and not of the program — the
default invocation reads the program alone, so it says the same thing on every
machine.

With --explain, every LLM node whose source-resolved backend is claude_code gets
its tool-surface reading: the native tools its tools: declaration grants and the
ones the backend withholds whatever the list says, with the reason per name —
the capability drift a declared list cannot see from the DSL alone (#2815). A
reading, never a diagnostic: it does not move the verdict.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunValidateWithContext(cmd.Context(), args[0], newPrinter(), validateOpts)
	},
}

func init() {
	validateCmd.Flags().BoolVar(&validateOpts.Exec, "exec", false, "after a clean compile, run the program under a dry run and report what it met")
	validateCmd.Flags().StringVar(&validateOpts.Fixtures, "fixtures", "", "JSON file of node outputs the dry run answers with ({node: output}, or a list of {node, output}); implies --exec")
	validateCmd.Flags().BoolVar(&validateOpts.Strict, "strict", false, "with --exec: exit non-zero when the dry run's report is failing — a pass died, or a reference, shell or fixture finding stands; an expression left inconclusive on a shape is printed, not failed (a CI gate); implies --exec")
	validateCmd.Flags().DurationVar(&validateOpts.ExecTimeout, "exec-timeout", 0, "with --exec: the bound of one pass of the dry run, the simulated children included (default 1m); a pass that runs out of time is said so in the report")
	validateCmd.Flags().IntVar(&validateOpts.ExecLoopCrossings, "exec-loop-crossings", 0, "with --exec: how often a pass crosses one bounded loop before the dry run declines its back-edge (default 3); a loop cut short is said so in the report, and a death that follows is the bound's, not the program's; a negative value removes the bound (loops run to their cap)")
	validateCmd.Flags().StringArrayVar(&validateOpts.Vars, "var", nil, "with --exec: a launch value for a workflow var (key=value, repeatable), with the precedence `run` has; implies --exec")
	validateCmd.Flags().StringVar(&validateOpts.Preset, "preset", "", "with --exec: an in-source named preset applied before --var; implies --exec")
	validateCmd.Flags().BoolVar(&validateOpts.Connectors, "connectors", false, "after a clean compile, resolve every `action:` node against the connector catalogue installed on this machine and name what it cannot serve; the findings are the install's state, so they do not change the verdict or the exit code")
	validateCmd.Flags().BoolVar(&validateOpts.Explain, "explain", false, "for every LLM node resolving to claude_code, show the native tools its tools: declaration grants and the ones the backend withholds whatever the list says, with the reason per name; a reading, never a diagnostic — it does not change the verdict or the exit code")
	rootCmd.AddCommand(validateCmd)
}
