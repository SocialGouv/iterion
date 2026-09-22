package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var fmtOpts cli.FmtOptions

// fmtCmd rewrites .bot files in their canonical form — the text the studio
// saves — proving before each write that the text reads as the same
// program, and refusing by name what it cannot rewrite without changing.
var fmtCmd = &cobra.Command{
	Use:   "fmt <file.bot | bundle dir | dir>...",
	Short: "Rewrite .bot files in their canonical form, provably the same program",
	Long: "Rewrite each .bot file in its canonical form: the text the studio saves,\n" +
		"proven before it is written to read as the same program (same parse, same\n" +
		"profile, same compiled workflow and diagnostics, prompt bodies canonical).\n\n" +
		"A comment goes back where it was written — its own indentation and its\n" +
		"paragraph breaks included — and the proof compares them too. A file that\n" +
		"cannot be rewritten without changing it is refused by name and left as it\n" +
		"is — one that does not parse, one whose comments the round trip would\n" +
		"lose, one holding a value written over several lines the writer has no\n" +
		"form for — and the others are formatted all the same. Archives (.botz)\n" +
		"are not formatted in place. --check writes nothing and exits non-zero\n" +
		"when a file would change or is refused.\n\n" +
		"--baseline names a file listing the paths this tree already knows are\n" +
		"refused: --check then passes while the refusals are exactly those, and\n" +
		"names the difference in either direction otherwise.",
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fmtOpts.Paths = args
		fmtOpts.Printer = newPrinter()
		_, err := cli.RunFmt(fmtOpts)
		return err
	},
}

func init() {
	fmtCmd.Flags().BoolVar(&fmtOpts.Check, "check", false, "Write nothing; exit non-zero when a file would change or is refused")
	fmtCmd.Flags().StringVar(&fmtOpts.Baseline, "baseline", "", "File listing the paths this tree already knows are refused: the check passes while the refusals are exactly those, and names the difference otherwise")
	rootCmd.AddCommand(fmtCmd)
}
