package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var secretOpts cli.SecretOptions

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage local (desktop / headless) secrets",
	Long: `Manage the local sealed secret store used by bots run outside cloud mode.

Secrets are AES-GCM sealed at rest under ~/.iterion/secrets.json (machine
global) or <store-dir>/.iterion/secrets.json (per-project, with --project),
keyed by a master key held in the OS keychain (fallback: ~/.iterion/secrets.key,
0600). A bot references a secret via its ` + "`secrets:`" + ` block and
` + "`{{secrets.NAME}}`" + `; the value is injected at tool/shell exec time and
never enters the agent's context. Values are never printed by any subcommand.

A stored value is shape-checked at ingestion, the same gate the cloud API
runs: a bearer token is one run of visible characters, so a pasted terminal
transcript is refused at the paste instead of surfacing as a provider 401
mid-run. The shape is read off the value (a PEM header, a JSON opener, else
a token) or named with ` + "`--kind`" + `; ` + "`--kind raw`" + ` stores anything.

Examples:
  iterion secret set GITHUB_TOKEN                 # masked prompt
  iterion secret set STRIPE_KEY --from-env SK     # import from an env var
  iterion secret set DB_URL --project --hosts db.internal
  iterion secret set DB_PASSPHRASE --kind raw     # not a token/JSON/PEM
  iterion secret list
  iterion secret rm GITHUB_TOKEN`,
}

var secretSetCmd = &cobra.Command{
	Use:   "set <NAME>",
	Short: "Store or rotate a secret (value read from prompt / stdin / --from-env)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		secretOpts.Name = args[0]
		secretOpts.HostsSet = cmd.Flags().Changed("hosts")
		return cli.RunSecretSet(newPrinter(), secretOpts)
	},
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List local secrets (names + last4 only, never values)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunSecretList(newPrinter(), secretOpts)
	},
}

var secretRmCmd = &cobra.Command{
	Use:   "rm <NAME>",
	Short: "Remove a local secret",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		secretOpts.Name = args[0]
		return cli.RunSecretRemove(newPrinter(), secretOpts)
	},
}

func init() {
	secretCmd.PersistentFlags().StringVar(&secretOpts.StoreDir, "store-dir", "", "Run store directory override (default: managed store for the working directory)")
	secretCmd.PersistentFlags().BoolVar(&secretOpts.Project, "project", false, "Target the per-project store (overrides global by name)")
	secretSetCmd.Flags().StringVar(&secretOpts.FromEnv, "from-env", "", "Read the value from this environment variable instead of prompting")
	secretSetCmd.Flags().StringSliceVar(&secretOpts.Hosts, "hosts", nil, "Egress host allowlist for the secret (comma-separated); empty = unrestricted")
	secretSetCmd.Flags().StringVar(&secretOpts.Kind, "kind", "", "Shape to validate the value against: token|json|pem|raw (default: inferred from the value; raw skips the check)")

	secretCmd.AddCommand(secretSetCmd, secretListCmd, secretRmCmd)
	rootCmd.AddCommand(secretCmd)
}
