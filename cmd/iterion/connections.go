package main

import (
	"os"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var connectionsCmd = &cobra.Command{
	Use:   "connections",
	Short: "Authenticate this machine to a connector",
	Long: `Bind a credential to a connector package, so a workflow can call it.

A connector package says WHAT a vendor offers; a connection says which
instance, with whose credential, and what it may be used for. A ` + "`.bot`" + ` names
one by its alias:

    tool comment:
      action: forgejo.issue.comment
      connection: main

The credential is read from an ENVIRONMENT VARIABLE, never a flag: a token
passed as an argument lands in your shell history and is visible in ` + "`ps`" + ` for
the life of the process. It is sealed with the same local master key as
` + "`iterion secret`" + ` and stored in the run store's connections.json.

A connection also carries its CAPABILITIES — what it may serve. The default is
` + "`action`" + ` alone (deterministic nodes, where the workflow decides every call).
Add ` + "`agent`" + ` to let a model choose calls through the MCP facade; that is a
wider grant and is therefore never the default.`,
}

var connectionsAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Store a credential for a connector",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		var o cli.ConnectionAddOptions
		o.Connector, _ = cmd.Flags().GetString("connector")
		o.Alias, _ = cmd.Flags().GetString("alias")
		o.BaseURL, _ = cmd.Flags().GetString("base-url")
		o.Scheme, _ = cmd.Flags().GetString("scheme")
		o.TokenEnv, _ = cmd.Flags().GetString("token-env")
		o.Capabilities, _ = cmd.Flags().GetStringSlice("capability")
		o.DisplayName, _ = cmd.Flags().GetString("name")
		o.StoreDir = connectionsStoreDir
		return cli.ConnectionsAdd(o, os.Stdout)
	},
}

var connectionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List this machine's connections",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cli.ConnectionsList(connectionsStoreDir, os.Stdout)
	},
}

var connectionsRemoveCmd = &cobra.Command{
	Use:   "rm <connector> <alias>",
	Short: "Remove a connection",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.ConnectionsRemove(connectionsStoreDir, args[0], args[1], os.Stdout)
	},
}

// connectionsStoreDir is the --store-dir override, resolved into the managed
// store for the working directory by pkg/cli.
var connectionsStoreDir string

func init() {
	connectionsCmd.PersistentFlags().StringVar(&connectionsStoreDir, "store-dir", "",
		"Run store directory override (default: the managed store for the working directory)")
	connectionsAddCmd.Flags().String("connector", "", "connector package id, e.g. forgejo")
	connectionsAddCmd.Flags().String("alias", "main", "the name a .bot writes in `connection:`")
	connectionsAddCmd.Flags().String("base-url", "", "the instance to authenticate to (default: the package's own)")
	connectionsAddCmd.Flags().String("scheme", "", "which of the package's auth schemes the credential satisfies (required when it declares several)")
	connectionsAddCmd.Flags().String("token-env", "", "name of the environment variable holding the credential")
	connectionsAddCmd.Flags().StringSlice("capability", nil, "what the connection may serve: action (default), agent")
	connectionsAddCmd.Flags().String("name", "", "display name")

	connectionsCmd.AddCommand(connectionsAddCmd, connectionsListCmd, connectionsRemoveCmd)
	rootCmd.AddCommand(connectionsCmd)
}
