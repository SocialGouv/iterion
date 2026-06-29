package main

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Manage iterion plugins (rewriters, MCP servers, skills, lifecycle)",
	Long: `Manage the iterion plugin ecosystem. A plugin is a declarative package
(plugin.yaml) that contributes typed extension points to the runtime:

  rewriter   command-output compressors (rtk ships as the default-enabled one)
  mcp        MCP servers (e.g. knowledge-graph explorers like repo-falcon)
  skill      markdown skills mirrored into the workspace
  lifecycle  index/refresh commands (e.g. build a code graph)

Builtins (rtk, graphify, repo-falcon) are embedded in the binary; rtk is
enabled by default, the knowledge-graph explorers ship disabled. Third-party
plugins install under ~/.iterion/plugins/. Enable/disable state lives in
~/.iterion/plugins.yaml.

  iterion plugin list                 # show all plugins + enable state
  iterion plugin enable repo-falcon   # turn a plugin on
  iterion plugin run repo-falcon index  # run a lifecycle command
  iterion plugin install <path|git-url> # install a third-party plugin`,
}

var pluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all plugins and their enable state",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		views, err := cli.PluginList()
		if err != nil {
			return err
		}
		p := newPrinter()
		if p.Format == cli.OutputJSON {
			p.JSON(map[string]any{"plugins": views})
			return nil
		}
		p.Header(fmt.Sprintf("%d plugin(s)", len(views)))
		for _, v := range views {
			state := "disabled"
			if v.Enabled {
				state = "enabled"
			}
			origin := "installed"
			if v.Builtin {
				origin = "builtin"
			}
			p.Line("  %-14s %-9s %-10s [%s] %s",
				v.Name, state, origin, strings.Join(v.Kinds, ","), v.Description)
		}
		return nil
	},
}

var pluginInfoCmd = &cobra.Command{
	Use:   "info <name>",
	Short: "Show a plugin's manifest details",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		view, manifest, err := cli.PluginInfo(args[0])
		if err != nil {
			return err
		}
		p := newPrinter()
		if p.Format == cli.OutputJSON {
			p.JSON(map[string]any{"plugin": view, "manifest": manifest})
			return nil
		}
		p.Header(view.Name)
		p.Line("  version:     %s", view.Version)
		p.Line("  description: %s", view.Description)
		p.Line("  enabled:     %t", view.Enabled)
		p.Line("  builtin:     %t", view.Builtin)
		p.Line("  contributes: %s", strings.Join(view.Kinds, ", "))
		return nil
	},
}

func pluginSetEnabledCmd(use string, enabled bool) *cobra.Command {
	verb := "Enable"
	if !enabled {
		verb = "Disable"
	}
	return &cobra.Command{
		Use:   use + " <name>",
		Short: verb + " a plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := cli.PluginSetEnabled(args[0], enabled); err != nil {
				return err
			}
			newPrinter().Line(fmt.Sprintf("plugin %q %sd", args[0], strings.ToLower(verb)))
			return nil
		},
	}
}

var pluginRunCmd = &cobra.Command{
	Use:   "run <name> <index|refresh>",
	Short: "Run a plugin's lifecycle command in the current workspace",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		workdir, _ := cmd.Flags().GetString("workdir")
		return cli.PluginRun(cmd.Context(), args[0], args[1], workdir)
	},
}

var pluginInstallCmd = &cobra.Command{
	Use:   "install <path|git-url>",
	Short: "Install a third-party plugin (or a bare public skill library) from a directory or git URL",
	Long: `Install a plugin from a local directory or git URL into
~/.iterion/plugins/. A source carrying a plugin.yaml installs as-is. A source
with no plugin.yaml but bare skills (a public skill library) is installed with
a synthesized skills-only manifest — every *.md under skills/ (or top-level
*.md when there is no skills/ dir) becomes a skill. Installed plugins are
disabled by default; enable with 'iterion plugin enable <name>'.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := cli.PluginInstall(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		newPrinter().Line(fmt.Sprintf("installed plugin %q (disabled by default unless its manifest opts in) — enable with: iterion plugin enable %s", name, name))
		return nil
	},
}

var pluginUninstallCmd = &cobra.Command{
	Use:   "uninstall <name>",
	Short: "Uninstall an installed plugin (builtins cannot be removed — disable them)",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		if err := cli.PluginUninstall(args[0]); err != nil {
			return err
		}
		newPrinter().Line(fmt.Sprintf("uninstalled plugin %q", args[0]))
		return nil
	},
}

func init() {
	pluginRunCmd.Flags().String("workdir", "", "Workspace directory the lifecycle command runs in (default: cwd)")
	pluginCmd.AddCommand(
		pluginListCmd,
		pluginInfoCmd,
		pluginSetEnabledCmd("enable", true),
		pluginSetEnabledCmd("disable", false),
		pluginRunCmd,
		pluginInstallCmd,
		pluginUninstallCmd,
	)
	rootCmd.AddCommand(pluginCmd)
}
