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
		printPluginConfig(p, view)
		return nil
	},
}

// printPluginConfig renders a plugin's config schema + current values (secret
// values shown only as (set)/(unset) — they never leave the server).
func printPluginConfig(p *cli.Printer, v cli.PluginView) {
	if len(v.ConfigSchema) == 0 {
		return
	}
	secret := map[string]bool{}
	for _, k := range v.ConfigSecretSet {
		secret[k] = true
	}
	p.Line("  config:")
	for _, f := range v.ConfigSchema {
		typ := f.Type
		if typ == "" {
			typ = "string"
		}
		var val string
		switch {
		case f.Type == "secret":
			if secret[f.Key] {
				val = "(set)"
			} else {
				val = "(unset)"
			}
		case v.ConfigValues[f.Key] != "":
			val = v.ConfigValues[f.Key]
		default:
			val = "(unset)"
		}
		req := ""
		if f.Required {
			req = " [required]"
		}
		p.Line("    %-16s %-7s = %s%s", f.Key, typ, val, req)
	}
}

var pluginConfigCmd = &cobra.Command{
	Use:   "config <name> [--set key=value ...]",
	Short: "Show or set a plugin's configuration",
	Long: `Show a plugin's declared configuration and current values, or set values
with one or more --set key=value flags. Secret values are never printed (only
whether they are set); set a secret with --set, and leave it unset to keep the
prior value. Values are stored in ~/.iterion/plugins.yaml and substituted into
the plugin's mcp/rewriter/lifecycle commands via {{config.<key>}}.

  iterion plugin config graphify
  iterion plugin config graphify --set max_depth=5 --set include_tests=true`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		sets, _ := cmd.Flags().GetStringArray("set")
		view, err := cli.PluginConfigView(name)
		if err != nil {
			return err
		}
		if len(sets) > 0 {
			if view, err = cli.PluginConfigSet(name, sets); err != nil {
				return err
			}
		}
		p := newPrinter()
		if p.Format == cli.OutputJSON {
			p.JSON(map[string]any{"plugin": view})
			return nil
		}
		if len(view.ConfigSchema) == 0 {
			p.Line("plugin %q declares no configuration", name)
			return nil
		}
		p.Header(view.Name)
		printPluginConfig(p, view)
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
	pluginConfigCmd.Flags().StringArray("set", nil, "Set a config value as key=value (repeatable)")
	pluginCmd.AddCommand(
		pluginListCmd,
		pluginInfoCmd,
		pluginConfigCmd,
		pluginSetEnabledCmd("enable", true),
		pluginSetEnabledCmd("disable", false),
		pluginRunCmd,
		pluginInstallCmd,
		pluginUninstallCmd,
	)
	rootCmd.AddCommand(pluginCmd)
}
