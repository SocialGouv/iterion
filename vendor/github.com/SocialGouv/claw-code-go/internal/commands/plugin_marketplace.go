package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/internal/plugins"
)

// PluginManagerProvider is the seam between the slash-command surface
// and the plugin manager. Adapters implement this so commands don't
// need to import the plugins package's full API.
type PluginManagerProvider interface {
	PluginManager() *plugins.Manager
}

// RegisterPluginMarketplaceCommands adds /store install|uninstall|
// list|search commands. /store is named separately from /plugin (the
// in-process plugin runtime) because the two manage different things:
// /plugin enables/disables already-loaded plugins; /store installs
// from a remote marketplace.
func RegisterPluginMarketplaceCommands(r *Registry) {
	r.Register(Command{
		Name:         "store",
		Description:  "Manage marketplace plugins (install / uninstall / list / search)",
		ArgumentHint: "[install|uninstall|list|search] <name|query>",
		Category:     CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			parts := strings.Fields(args)
			sub := "list"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}
			provider, ok := loop.(PluginManagerProvider)
			if !ok || provider.PluginManager() == nil {
				fmt.Println("Plugin manager not available in this context.")
				return nil
			}
			mgr := provider.PluginManager()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			switch sub {
			case "list", "":
				rows, err := mgr.List()
				if err != nil {
					return fmt.Errorf("plugins list: %w", err)
				}
				if len(rows) == 0 {
					fmt.Println("No plugins installed.")
					return nil
				}
				fmt.Println("Installed plugins:")
				for _, p := range rows {
					if p.Description != "" {
						fmt.Printf("  %s @ %s — %s\n", p.Name, p.Version, p.Description)
					} else {
						fmt.Printf("  %s @ %s\n", p.Name, p.Version)
					}
				}

			case "install":
				if len(parts) < 2 {
					fmt.Println("Usage: /store install <name>")
					return nil
				}
				row, err := mgr.Install(ctx, parts[1])
				if err != nil {
					return fmt.Errorf("plugins install: %w", err)
				}
				fmt.Printf("Installed %s @ %s\n", row.Name, row.Version)

			case "uninstall":
				if len(parts) < 2 {
					fmt.Println("Usage: /store uninstall <name>")
					return nil
				}
				if err := mgr.Uninstall(ctx, parts[1]); err != nil {
					return fmt.Errorf("plugins uninstall: %w", err)
				}
				fmt.Printf("Uninstalled %s\n", parts[1])

			case "search":
				query := ""
				if len(parts) > 1 {
					query = strings.Join(parts[1:], " ")
				}
				hits, err := mgr.Search(ctx, query)
				if err != nil {
					return fmt.Errorf("plugins search: %w", err)
				}
				if len(hits) == 0 {
					fmt.Println("No plugins matched.")
					return nil
				}
				for _, e := range hits {
					if e.Description != "" {
						fmt.Printf("  %s @ %s — %s\n", e.Name, e.Version, e.Description)
					} else {
						fmt.Printf("  %s @ %s\n", e.Name, e.Version)
					}
				}

			default:
				fmt.Printf("Unknown subcommand: %s\n", sub)
				fmt.Println("Usage: /store [install|uninstall|list|search] <name|query>")
			}
			return nil
		},
	})
}
