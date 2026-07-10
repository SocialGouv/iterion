package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote secrets / api-keys / bindings — sealed credential stores.
// Secret VALUES are read from --from-env/--from-file/stdin only.

var (
	remoteSecretScope    string
	remoteSecretFromEnv  string
	remoteSecretFromFile string
)

func remoteScopedBase(cmd *cobra.Command, c *cli.RemoteClient, resource string) (string, error) {
	switch remoteSecretScope {
	case "", "team":
		return teamBase(cmd, c, "/"+resource)
	case "me":
		return "/api/me/" + resource, nil
	default:
		return "", fmt.Errorf("invalid --scope %q (want team|me)", remoteSecretScope)
	}
}

var remoteSecretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Generic named secrets (team or personal scope)",
}

var remoteSecretsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secrets (metadata only — values are sealed)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := remoteScopedBase(cmd, c, "secrets")
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, p, base)
	}),
}

var remoteSecretsSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Create a secret (value from --from-env/--from-file/stdin)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		value, err := cli.ReadSecretValue(remoteSecretFromEnv, remoteSecretFromFile, true)
		if err != nil {
			return err
		}
		return cli.RemoteSecretsSet(cmd.Context(), c, p, remoteSecretScope, remoteTeamFlag, args[0], value)
	}),
}

var remoteSecretsRotateCmd = &cobra.Command{
	Use:   "rotate <secret-id>",
	Short: "Rotate a secret's value",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		value, err := cli.ReadSecretValue(remoteSecretFromEnv, remoteSecretFromFile, true)
		if err != nil {
			return err
		}
		return cli.RemoteSecretsRotate(cmd.Context(), c, p, remoteSecretScope, remoteTeamFlag, args[0], value)
	}),
}

var remoteSecretsDeleteCmd = &cobra.Command{
	Use:   "delete <secret-id>",
	Short: "Delete a secret",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := remoteScopedBase(cmd, c, "secrets")
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[0], nil)
	}),
}

// --- BYOK api-keys ---

var remoteAPIKeysCmd = &cobra.Command{
	Use:   "api-keys",
	Short: "BYOK LLM provider API keys (team or personal scope)",
}

var remoteAPIKeysListCmd = &cobra.Command{
	Use:   "list",
	Short: "List API keys (metadata only)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := remoteScopedBase(cmd, c, "api-keys")
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, p, base)
	}),
}

var (
	remoteAPIKeyProvider string
	remoteAPIKeyName     string
	remoteAPIKeyDefault  bool
)

var remoteAPIKeysCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Add an API key (--provider + --name; value from --from-env/--from-file/stdin)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remoteAPIKeyProvider == "" || remoteAPIKeyName == "" {
			return fmt.Errorf("--provider and --name are required")
		}
		value, err := cli.ReadSecretValue(remoteSecretFromEnv, remoteSecretFromFile, true)
		if err != nil {
			return err
		}
		return cli.RemoteAPIKeysCreate(cmd.Context(), c, p, remoteSecretScope, remoteTeamFlag, remoteAPIKeyProvider, remoteAPIKeyName, value, remoteAPIKeyDefault)
	}),
}

var remoteAPIKeysUpdateData string

var remoteAPIKeysUpdateCmd = &cobra.Command{
	Use:   "update <key-id>",
	Short: "Update an API key's metadata (--data JSON; rotate via secrets fields)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := remoteScopedBase(cmd, c, "api-keys")
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", base+"/"+args[0], remoteAPIKeysUpdateData, "patch JSON")
	}),
}

var remoteAPIKeysDeleteCmd = &cobra.Command{
	Use:   "delete <key-id>",
	Short: "Delete an API key",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := remoteScopedBase(cmd, c, "api-keys")
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[0], nil)
	}),
}

// --- bot-secret bindings (team-scoped only) ---

var remoteBindingsData string

var remoteBindingsCmd = &cobra.Command{
	Use:   "bindings <bot-id> [create|delete <binding-id>]",
	Short: "Bot↔secret bindings for a bot (team scope)",
	Args:  cobra.RangeArgs(1, 3),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/bots/"+args[0]+"/bindings")
		if err != nil {
			return err
		}
		switch {
		case len(args) == 1:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[1] == "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteBindingsData, "binding JSON")
		case args[1] == "delete" && len(args) == 3:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[2], nil)
		default:
			return fmt.Errorf("usage: bindings <bot-id> [create --data @f|delete <binding-id>]")
		}
	}),
}

func init() {
	for _, c := range []*cobra.Command{
		remoteSecretsListCmd, remoteSecretsSetCmd, remoteSecretsRotateCmd, remoteSecretsDeleteCmd,
		remoteAPIKeysListCmd, remoteAPIKeysCreateCmd, remoteAPIKeysUpdateCmd, remoteAPIKeysDeleteCmd,
	} {
		c.Flags().StringVar(&remoteSecretScope, "scope", "team", "Store scope (team|me)")
		c.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")
	}
	for _, c := range []*cobra.Command{remoteSecretsSetCmd, remoteSecretsRotateCmd, remoteAPIKeysCreateCmd} {
		c.Flags().StringVar(&remoteSecretFromEnv, "from-env", "", "Read the value from this environment variable")
		c.Flags().StringVar(&remoteSecretFromFile, "from-file", "", "Read the value from this file")
	}
	remoteAPIKeysCreateCmd.Flags().StringVar(&remoteAPIKeyProvider, "provider", "", "LLM provider (anthropic|openai|…)")
	remoteAPIKeysCreateCmd.Flags().StringVar(&remoteAPIKeyName, "name", "", "Key display name")
	remoteAPIKeysCreateCmd.Flags().BoolVar(&remoteAPIKeyDefault, "default", false, "Make this the provider's default key")
	remoteAPIKeysUpdateCmd.Flags().StringVar(&remoteAPIKeysUpdateData, "data", "", "Patch JSON (literal or @file)")
	remoteBindingsCmd.Flags().StringVar(&remoteBindingsData, "data", "", "Binding JSON for create (literal or @file)")
	remoteBindingsCmd.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")

	remoteSecretsCmd.AddCommand(remoteSecretsListCmd, remoteSecretsSetCmd, remoteSecretsRotateCmd, remoteSecretsDeleteCmd)
	remoteAPIKeysCmd.AddCommand(remoteAPIKeysListCmd, remoteAPIKeysCreateCmd, remoteAPIKeysUpdateCmd, remoteAPIKeysDeleteCmd)
	remoteCmd.AddCommand(remoteSecretsCmd, remoteAPIKeysCmd, remoteBindingsCmd)
}
