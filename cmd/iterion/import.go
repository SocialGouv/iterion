package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var importOpts struct {
	out    string
	name   string
	dryRun bool
}

var importCmd = &cobra.Command{
	Use:   "import <workflow.js>",
	Short: "Convert a Claude-Code workflow script (.js) into a draft .bot",
	Long: `Convert a Claude-Code workflow script (.claude/workflows/*.js — the
export const meta + agent()/phase()/log() shape) into a DRAFT .bot.

The conversion is lossy by design and never executes any JavaScript:
recognized constructs become real DSL (agents, schemas, prompts,
bounded loops, conditional exits, fan-outs); everything else becomes
an annotated ## IMPORT marker plus a report entry. The draft always
starts with an ## IMPORT REPORT header and is compile-checked before
it is written. See docs/import.md.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunImport(cli.ImportOptions{
			File:   args[0],
			Out:    importOpts.out,
			Name:   importOpts.name,
			DryRun: importOpts.dryRun,
		}, newPrinter())
	},
}

func init() {
	f := importCmd.Flags()
	f.StringVar(&importOpts.out, "out", "", "Output .bot path (default: <workflow-name>.bot next to the source)")
	f.StringVar(&importOpts.name, "name", "", "Override the workflow name (default: meta.name, then the file stem)")
	f.BoolVar(&importOpts.dryRun, "dry-run", false, "Print the draft instead of writing it")
	rootCmd.AddCommand(importCmd)
}
