package main

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var botsCmd = &cobra.Command{
	Use:   "bots",
	Short: "Create, install, and describe bots",
	Long: `Create new bots, install published ones, and emit the catalog.

"create" scaffolds a new bot bundle from a template — the CLI half of the
studio builder at /bots/new. "install" imports a published bundle. "list"
discovers .bot files and bundle directories on disk and emits a structured
catalog used by orchestrator bots (e.g. whats-next) to pick the right bot
for an issue. Output formats: json (default), markdown, skill.

The "skill" format emits a SKILL.md ready to drop into a bundle's skills/
directory; that's the canonical way to refresh bots/whats-next/skills/
iterion-bot-catalog.md after adding or renaming a bot.`,
}

var botsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List bots discovered under one or more paths",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		paths, _ := cmd.Flags().GetStringSlice("paths")
		format, _ := cmd.Flags().GetString("format")
		if len(paths) == 0 {
			paths = []string{"bots", "examples"}
		}
		return cli.BotsList(cli.BotsListOptions{Paths: paths, Format: format, ErrW: os.Stderr}, os.Stdout)
	},
}

var botsRegenCatalogCmd = &cobra.Command{
	Use:   "regen-catalog",
	Short: "Regenerate the whats-next bot catalog from bot manifests",
	Long: `Rebuild the generated region of bots/whats-next/skills/
iterion-bot-catalog.md (persona table + per-bot cards) from every bot's
manifest.yaml under the workspace, applying the .iterion/bot-overrides.yaml
catalog overlay. The runtime does this automatically at whats-next start and
the studio on every bot-metadata save; use this to refresh the committed copy
by hand after editing a manifest outside the studio.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		workdir, _ := cmd.Flags().GetString("workdir")
		if workdir == "" {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			workdir = wd
		}
		path, err := cli.BotsRegenCatalog(workdir)
		if err != nil {
			return err
		}
		if path == "" {
			fmt.Fprintf(os.Stderr, "no whats-next catalog template under %s — nothing to regenerate\n", workdir)
			return nil
		}
		fmt.Fprintln(os.Stdout, "regenerated", path)
		return nil
	},
}

var botsCreateCmd = &cobra.Command{
	Use:   "create <slug>",
	Short: "Scaffold a new bot bundle from a template",
	Long: `Scaffold a new bot bundle (main.bot + manifest.yaml + README.md + the
bundle layout directories) under bots/<slug>, then refresh the generated bot
catalog so orchestrators can route to it.

This is the CLI half of the studio's "New bot" builder (/bots/new) — both
render through the same engine, so a bot created either way is identical. The
generated main.bot follows the house shape: ONE adaptive agent carrying the
whole mission, with worktree/sandbox as opt-in workflow dials.

Start from a gallery template with --template (see "iterion bots templates");
every field stays editable afterwards. The rendered workflow is parsed AND
compiled before anything is written, so a scaffold never lands a broken
bundle.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.BotsCreateOptions{Slug: args[0]}
		opts.Workdir, _ = cmd.Flags().GetString("workdir")
		opts.Dest, _ = cmd.Flags().GetString("dest")
		opts.Template, _ = cmd.Flags().GetString("template")
		opts.DisplayName, _ = cmd.Flags().GetString("display-name")
		opts.Description, _ = cmd.Flags().GetString("description")
		opts.Instructions, _ = cmd.Flags().GetString("instructions")
		opts.Model, _ = cmd.Flags().GetString("model")
		opts.Backend, _ = cmd.Flags().GetString("backend")
		// Only an explicitly-passed dial overrides the template's value.
		opts.Worktree = boolFlagIfSet(cmd, "worktree")
		opts.Sandbox = boolFlagIfSet(cmd, "sandbox")
		return cli.BotsCreate(opts, newPrinter())
	},
}

// boolFlagIfSet returns the flag's value only when the operator passed it
// explicitly, so an unset flag stays distinguishable from an explicit
// false and cannot silently override a template's own dial.
func boolFlagIfSet(cmd *cobra.Command, name string) *bool {
	if !cmd.Flags().Changed(name) {
		return nil
	}
	v, _ := cmd.Flags().GetBool(name)
	return &v
}

var botsTemplatesCmd = &cobra.Command{
	Use:   "templates",
	Short: "List the templates `bots create` can start from",
	Long: `List the bot-creation gallery — the same templates the studio builder
offers at /bots/new. Use one with "iterion bots create <slug> --template <id>".`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cli.BotsTemplates(newPrinter())
	},
}

var botsInstallCmd = &cobra.Command{
	Use:   "install <git-url|path>",
	Short: "Install a bot bundle from a git repository or local path",
	Long: `Import a bot bundle from a git URL (optionally URL#ref) or a local
directory into the workspace, so iterion bots list, the dispatcher, and the
studio discover it.

A source repository publishes bots by holding bundle directories (each a
main.bot + manifest.yaml) and, optionally, an iterion-bots.yaml index at its
root listing them. When the repo holds a single bundle it is installed
directly; when it holds several, pass --path <subdir|name> to choose one.

Installed bots are NEVER run automatically — inspect, then launch (run-time
sandboxing applies as usual). By default bots install under <workdir>/.botz/
(git-ignored); pass --dest bots to install into a committable location.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ref, _ := cmd.Flags().GetString("ref")
		path, _ := cmd.Flags().GetString("path")
		dest, _ := cmd.Flags().GetString("dest")
		name, _ := cmd.Flags().GetString("name")
		force, _ := cmd.Flags().GetBool("force")
		workdir, _ := cmd.Flags().GetString("workdir")
		p := newPrinter()
		res, err := botinstall.Install(cmd.Context(), botinstall.Options{
			Source: args[0], Ref: ref, Path: path, Dest: dest, Name: name, Force: force, Workdir: workdir,
		})
		if err != nil {
			return err
		}
		if p.Format == cli.OutputJSON {
			p.JSON(res)
			return nil
		}
		p.Header("Bot installed")
		p.KV("Name", res.Name)
		p.KV("From", res.Source)
		if res.Ref != "" {
			p.KV("Ref", res.Ref)
		}
		p.KV("Path", res.InstalledPath)
		p.KV("Skills", fmt.Sprintf("%d", res.Skills))
		p.KV("Presets", fmt.Sprintf("%d", res.Presets))
		p.Blank()
		p.Line("  Inspect it, then launch:")
		p.Line("    iterion run %s", res.InstalledPath)
		return nil
	},
}

func init() {
	botsListCmd.Flags().StringSlice("paths", nil, "Directories or .bot files to scan (default: bots, examples)")
	botsListCmd.Flags().String("format", "json", "Output format: json|markdown|skill")
	botsRegenCatalogCmd.Flags().String("workdir", "", "Workspace root to scan (default: current directory)")
	botsCreateCmd.Flags().String("template", "blank", "Template to start from (see `iterion bots templates`)")
	botsCreateCmd.Flags().String("workdir", "", "Workspace root anchoring --dest and the catalog refresh (default: current directory)")
	botsCreateCmd.Flags().String("dest", botregistry.BotsDirName, "Parent directory for the new bundle, relative to --workdir")
	botsCreateCmd.Flags().String("display-name", "", "Human-facing bot name")
	botsCreateCmd.Flags().String("description", "", "One-line description for the catalog")
	botsCreateCmd.Flags().String("instructions", "", "The agent's mission (its system prompt body)")
	botsCreateCmd.Flags().String("model", "", "Pin a model instead of auto-detection")
	botsCreateCmd.Flags().String("backend", "", "Pin a backend instead of auto-detection")
	botsCreateCmd.Flags().Bool("worktree", false, "Run in a dedicated git worktree")
	botsCreateCmd.Flags().Bool("sandbox", false, "Run in a sandboxed container")
	botsInstallCmd.Flags().String("ref", "", "Git ref (branch or tag) to clone")
	botsInstallCmd.Flags().String("path", "", "Subdirectory or iterion-bots.yaml bot name to install when the repo holds several")
	botsInstallCmd.Flags().String("dest", "", "Install destination root (default: <workdir>/.botz)")
	botsInstallCmd.Flags().String("name", "", "Install under this name instead of the source's")
	botsInstallCmd.Flags().Bool("force", false, "Overwrite an existing install of the same name")
	botsInstallCmd.Flags().String("workdir", "", "Workspace root for catalog refresh (default: current directory)")
	botsCmd.AddCommand(botsCreateCmd)
	botsCmd.AddCommand(botsTemplatesCmd)
	botsCmd.AddCommand(botsListCmd)
	botsCmd.AddCommand(botsRegenCatalogCmd)
	botsCmd.AddCommand(botsInstallCmd)
	rootCmd.AddCommand(botsCmd)
}
