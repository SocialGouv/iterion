package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var skillOpts cli.SkillOptions

var skillCmd = &cobra.Command{
	Use:   "skill",
	Short: "Manage the local skill library",
	Long: `Manage the local skill library — a curated store of Claude-Code-style
SKILL.md skills, referenceable from any workflow via the DSL ` + "`skills:`" + ` field.

Skills live under ~/.iterion/skills/<name>/SKILL.md (machine global) or
<store-dir>/.iterion/skills/<name>/SKILL.md (per-project, with --project, which
shadows the global by name). A workflow node opts in with, e.g.,
` + "`skills: [\"changelog-writer\"]`" + `; at run start iterion mirrors each referenced
skill into the workspace's .claude/skills/ (where both claude_code and claw
discover it) and lists it under a "## Skills" system-prompt section.

Third-party skill packs (a bare skills/ git repo) install via
` + "`iterion skill import <git-url>`" + `, which delegates to the plugin path.

Examples:
  iterion skill add changelog-writer --from ./changelog-writer.md
  iterion skill add house-style --project        # body from stdin, project scope
  iterion skill list
  iterion skill show changelog-writer
  iterion skill import https://github.com/acme/awesome-claude-skills
  iterion skill rm changelog-writer`,
}

var skillListCmd = &cobra.Command{
	Use:   "list",
	Short: "List library skills (both scopes) with descriptions",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunSkillList(newPrinter(), skillOpts)
	},
}

var skillShowCmd = &cobra.Command{
	Use:   "show <NAME>",
	Short: "Print a skill's resolved path and full body",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		skillOpts.Name = args[0]
		return cli.RunSkillShow(newPrinter(), skillOpts)
	},
}

var skillAddCmd = &cobra.Command{
	Use:   "add <NAME>",
	Short: "Create or overwrite a skill (body from --from <file> or stdin)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		skillOpts.Name = args[0]
		return cli.RunSkillAdd(newPrinter(), skillOpts)
	},
}

var skillRmCmd = &cobra.Command{
	Use:   "rm <NAME>",
	Short: "Remove a skill at the selected scope",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		skillOpts.Name = args[0]
		return cli.RunSkillRemove(newPrinter(), skillOpts)
	},
}

var skillImportCmd = &cobra.Command{
	Use:   "import <git-url|path>",
	Short: "Install a public skill pack (bare skills/ repo) as an enable/disable-able plugin",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		skillOpts.Name = args[0]
		return cli.RunSkillImport(newPrinter(), skillOpts)
	},
}

var skillExportCmd = &cobra.Command{
	Use:   "export <NAME> [DIR]",
	Short: "Copy a skill's markdown out to a directory (default: cwd)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		skillOpts.Name = args[0]
		if len(args) > 1 {
			skillOpts.Dir = args[1]
		}
		return cli.RunSkillExport(newPrinter(), skillOpts)
	},
}

func init() {
	skillCmd.PersistentFlags().StringVar(&skillOpts.StoreDir, "store-dir", "", "Run store directory override (default: managed store for the working directory)")
	skillCmd.PersistentFlags().BoolVar(&skillOpts.Project, "project", false, "Target the per-project skill store (shadows global by name)")
	skillAddCmd.Flags().StringVar(&skillOpts.From, "from", "", "Read the skill body from this file (default: stdin)")

	skillCmd.AddCommand(skillListCmd, skillShowCmd, skillAddCmd, skillRmCmd, skillImportCmd, skillExportCmd)
	rootCmd.AddCommand(skillCmd)
}
