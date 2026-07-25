package main

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/spf13/cobra"
)

var marketplaceCmd = &cobra.Command{
	Use:   "marketplace",
	Short: "Browse, submit, and install bots and plugins from the local hosted registry",
	Long: `Operate the local marketplace — the same registry the studio's
Marketplace view reads (stored at <store-dir>/marketplace/marketplace.json).
Entries are bots (.bot/.botz bundles) or plugins (plugin.yaml packages);
submit auto-detects which kind a repo publishes.

  iterion marketplace list                 # browse the registry
  iterion marketplace list --kind plugin   # only plugin entries
  iterion marketplace submit <url|path>    # validate + index a repo (bot or plugin)
  iterion marketplace install <slug>       # install a listed entry
  iterion marketplace uninstall <slug>     # remove the installed artifact

Submitting only indexes a repo's metadata (it does not install). Installing
resolves the entry's repo coordinates: a bot's bundle is copied into the
workspace's .botz/; a plugin installs under ~/.iterion/plugins/. Bots are
never run automatically.`,
}

var marketplaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List entries in the local registry",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		storeDir, _ := cmd.Flags().GetString("store-dir")
		q, _ := cmd.Flags().GetString("query")
		tag, _ := cmd.Flags().GetString("tag")
		kind, _ := cmd.Flags().GetString("kind")
		entries, err := cli.MarketplaceList(cmd.Context(), cli.MarketplaceListOptions{
			StoreDir: storeDir, Text: q, Tag: tag, Kind: kind,
		})
		if err != nil {
			return err
		}
		p := newPrinter()
		if p.Format == cli.OutputJSON {
			p.JSON(map[string]any{"bots": entries})
			return nil
		}
		if len(entries) == 0 {
			p.Line("No entries in the marketplace yet.")
			return nil
		}
		p.Header(fmt.Sprintf("%d entry(ies)", len(entries)))
		for _, e := range entries {
			label := e.DisplayName
			if label == "" {
				label = e.Name
			}
			p.Line("  %-22s %-7s %3d install(s)  %s", e.Slug, marketplace.EffectiveKind(e), e.Installs, label)
			if e.Description != "" {
				p.Line("      %s", e.Description)
			}
			if len(e.Tags) > 0 {
				p.Line("      tags: %s", strings.Join(e.Tags, ", "))
			}
		}
		return nil
	},
}

var marketplaceSubmitCmd = &cobra.Command{
	Use:   "submit <git-url|path>",
	Short: "Validate a repository and index the bot or plugin it publishes",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		storeDir, _ := cmd.Flags().GetString("store-dir")
		ref, _ := cmd.Flags().GetString("ref")
		path, _ := cmd.Flags().GetString("path")
		tags, _ := cmd.Flags().GetStringSlice("tag")
		entry, err := cli.MarketplaceSubmit(cmd.Context(), cli.MarketplaceSubmitOptions{
			StoreDir: storeDir, Source: args[0], Ref: ref, Path: path, Tags: tags,
		})
		if err != nil {
			return err
		}
		p := newPrinter()
		if p.Format == cli.OutputJSON {
			p.JSON(entry)
			return nil
		}
		p.Header("Submitted to the marketplace")
		p.KV("Slug", entry.Slug)
		p.KV("Kind", string(marketplace.EffectiveKind(*entry)))
		p.KV("Name", entry.Name)
		if entry.Version != "" {
			p.KV("Version", entry.Version)
		}
		p.KV("Repo", entry.RepoURL)
		p.Blank()
		p.Line("  Install it with:")
		p.Line("    iterion marketplace install %s", entry.Slug)
		return nil
	},
}

var marketplaceInstallCmd = &cobra.Command{
	Use:   "install <slug>",
	Short: "Install a listed entry (bot → workspace .botz/, plugin → ~/.iterion/plugins/)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		storeDir, _ := cmd.Flags().GetString("store-dir")
		workdir, _ := cmd.Flags().GetString("workdir")
		force, _ := cmd.Flags().GetBool("force")
		res, err := cli.MarketplaceInstall(cmd.Context(), cli.MarketplaceInstallOptions{
			StoreDir: storeDir, Slug: args[0], Workdir: workdir, Force: force,
		})
		if err != nil {
			return err
		}
		p := newPrinter()
		if p.Format == cli.OutputJSON {
			p.JSON(map[string]any{"kind": res.Kind, "install": res.Bot, "plugin": res.Plugin, "entry": res.Entry})
			return nil
		}
		if res.Kind == marketplace.KindPlugin {
			p.Header("Plugin installed")
			p.KV("Name", res.Plugin)
			p.KV("Installs", fmt.Sprintf("%d", res.Entry.Installs))
			p.Blank()
			p.Line("  Enable it with:")
			p.Line("    iterion plugin enable %s", res.Plugin)
			return nil
		}
		p.Header("Bot installed")
		p.KV("Name", res.Bot.Name)
		p.KV("Path", res.Bot.InstalledPath)
		p.KV("Installs", fmt.Sprintf("%d", res.Entry.Installs))
		p.Blank()
		p.Line("  Inspect it, then launch:")
		p.Line("    iterion run %s", res.Bot.InstalledPath)
		return nil
	},
}

var marketplaceUninstallCmd = &cobra.Command{
	Use:   "uninstall <slug>",
	Short: "Remove a listed entry's installed artifact (bot bundle or plugin)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		storeDir, _ := cmd.Flags().GetString("store-dir")
		workdir, _ := cmd.Flags().GetString("workdir")
		entry, err := cli.MarketplaceUninstall(cmd.Context(), cli.MarketplaceUninstallOptions{
			StoreDir: storeDir, Slug: args[0], Workdir: workdir,
		})
		if err != nil {
			return err
		}
		p := newPrinter()
		if p.Format == cli.OutputJSON {
			p.JSON(map[string]any{"kind": marketplace.EffectiveKind(*entry), "entry": entry})
			return nil
		}
		if marketplace.EffectiveKind(*entry) == marketplace.KindPlugin {
			p.Line("uninstalled plugin %q", entry.Name)
			return nil
		}
		p.Line("uninstalled bot %q from the workspace's .botz/", entry.Name)
		return nil
	},
}

func init() {
	marketplaceListCmd.Flags().String("store-dir", "", "Store directory override (default: managed store for the working directory)")
	marketplaceListCmd.Flags().StringP("query", "q", "", "Free-text filter (name/description/tag)")
	marketplaceListCmd.Flags().String("tag", "", "Exact tag filter")
	marketplaceListCmd.Flags().String("kind", "", "Filter by artifact kind (bot|plugin)")

	marketplaceSubmitCmd.Flags().String("store-dir", "", "Store directory override (default: managed store for the working directory)")
	marketplaceSubmitCmd.Flags().String("ref", "", "Git ref (branch or tag) to clone")
	marketplaceSubmitCmd.Flags().String("path", "", "Subdirectory or iterion-bots.yaml bot name when the repo holds several")
	marketplaceSubmitCmd.Flags().StringSlice("tag", nil, "Marketplace tags (repeatable)")

	marketplaceInstallCmd.Flags().String("store-dir", "", "Store directory override (default: managed store for the working directory)")
	marketplaceInstallCmd.Flags().String("workdir", "", "Workspace root to install into (default: current directory; bots only)")
	marketplaceInstallCmd.Flags().Bool("force", false, "Overwrite an existing install (update)")

	marketplaceUninstallCmd.Flags().String("store-dir", "", "Store directory override (default: managed store for the working directory)")
	marketplaceUninstallCmd.Flags().String("workdir", "", "Workspace root to uninstall from (default: current directory; bots only)")

	marketplaceCmd.AddCommand(marketplaceListCmd)
	marketplaceCmd.AddCommand(marketplaceSubmitCmd)
	marketplaceCmd.AddCommand(marketplaceInstallCmd)
	marketplaceCmd.AddCommand(marketplaceUninstallCmd)
	rootCmd.AddCommand(marketplaceCmd)
}
