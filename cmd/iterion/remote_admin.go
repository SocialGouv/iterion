package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote admin / sso / plugins — platform administration. Admin
// endpoints are super-admin-gated server-side; the CLI does no
// client-side role check (a 403 surfaces as-is).

var remoteAdminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Platform administration (super-admin)",
}

var remoteAdminData string

var remoteAdminOrgsCmd = &cobra.Command{
	Use:   "orgs [get|create|update|delete|restore|status|teams|usage] [id] [status]",
	Short: "Org console: list (default) or act on one org",
	Args:  cobra.MaximumNArgs(3),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/orgs")
		}
		action := args[0]
		needID := func() (string, error) {
			if len(args) < 2 {
				return "", fmt.Errorf("usage: admin orgs %s <org-id>", action)
			}
			return args[1], nil
		}
		switch action {
		case "get":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/orgs/"+id)
		case "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", "/api/admin/orgs", remoteAdminData, "org JSON")
		case "update":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", "/api/admin/orgs/"+id, remoteAdminData, "patch JSON")
		case "delete":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/orgs/"+id, nil)
		case "restore":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/orgs/"+id+"/restore", nil)
		case "status":
			if len(args) != 3 {
				return fmt.Errorf("usage: admin orgs status <org-id> <status>")
			}
			body := []byte(fmt.Sprintf(`{"status":%q}`, args[2]))
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/orgs/"+args[1]+"/status", body)
		case "teams":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/orgs/"+id+"/teams")
		case "usage":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/orgs/"+id+"/usage")
		default:
			return fmt.Errorf("unknown orgs action %q", action)
		}
	}),
}

var remoteAdminUsersCmd = &cobra.Command{
	Use:   "users [update <user-id>]",
	Short: "List platform users, or update one (--data)",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/users")
		}
		if args[0] != "update" || len(args) != 2 {
			return fmt.Errorf("usage: admin users [update <user-id> --data @f]")
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", "/api/admin/users/"+args[1], remoteAdminData, "patch JSON")
	}),
}

var remoteAdminDLQCmd = &cobra.Command{
	Use:   "dlq [show|replay|delete <seq>]",
	Short: "Queue dead-letter entries: list (default) or act on one",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/dlq")
		}
		if len(args) != 2 {
			return fmt.Errorf("usage: admin dlq [show|replay|delete <seq>]")
		}
		switch args[0] {
		case "show":
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/dlq/"+args[1])
		case "replay":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/dlq/"+args[1]+"/replay", nil)
		case "delete":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/dlq/"+args[1], nil)
		default:
			return fmt.Errorf("unknown dlq action %q (want show|replay|delete)", args[0])
		}
	}),
}

// --- platform LLM credentials (DB-backed env fallback) ---

var (
	remoteLLMFromEnv  string
	remoteLLMFromFile string
	remoteLLMProvider string
	remoteLLMName     string
	remoteLLMDefault  bool
	remoteLLMKeyData  string
)

var remoteAdminLLMCmd = &cobra.Command{
	Use:   "llm",
	Short: "Platform LLM credentials — rotate the deployment's provider keys/forfait without a redeploy",
}

var remoteAdminLLMKeysCmd = &cobra.Command{
	Use:   "api-keys [create|rotate|update|delete] [key-id]",
	Short: "Platform provider API keys: list (default) or act on one (values from --from-env/--from-file/stdin)",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/llm/api-keys")
		}
		action := args[0]
		needID := func() (string, error) {
			if len(args) < 2 {
				return "", fmt.Errorf("usage: admin llm api-keys %s <key-id>", action)
			}
			return args[1], nil
		}
		switch action {
		case "create":
			if remoteLLMProvider == "" || remoteLLMName == "" {
				return fmt.Errorf("--provider and --name are required")
			}
			value, err := cli.ReadSecretValue(remoteLLMFromEnv, remoteLLMFromFile, true)
			if err != nil {
				return err
			}
			return cli.RemoteAPIKeysCreate(cmd.Context(), c, p, "platform", "", remoteLLMProvider, remoteLLMName, value, remoteLLMDefault)
		case "rotate":
			id, err := needID()
			if err != nil {
				return err
			}
			value, err := cli.ReadSecretValue(remoteLLMFromEnv, remoteLLMFromFile, true)
			if err != nil {
				return err
			}
			return cli.RemoteAPIKeysRotate(cmd.Context(), c, p, "platform", "", id, value)
		case "update":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", "/api/admin/llm/api-keys/"+id, remoteLLMKeyData, "patch JSON")
		case "delete":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/llm/api-keys/"+id, nil)
		default:
			return fmt.Errorf("unknown api-keys action %q (want create|rotate|update|delete)", action)
		}
	}),
}

var remoteAdminLLMOAuthCmd = &cobra.Command{
	Use:   "oauth [set|connect|refresh|delete] [kind]",
	Short: "Platform OAuth-forfait: list connections (default) or act on one kind (claude_code|codex)",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/llm/oauth/connections")
		}
		action := args[0]
		needKind := func() (string, error) {
			if len(args) < 2 {
				return "", fmt.Errorf("usage: admin llm oauth %s <kind>", action)
			}
			return args[1], nil
		}
		switch action {
		case "set":
			// The forfait blob (claude_code credentials.json / codex auth.json).
			kind, err := needKind()
			if err != nil {
				return err
			}
			blob, err := cli.ReadSecretBlob(remoteLLMFromEnv, remoteLLMFromFile)
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/llm/oauth/"+kind+"/credentials", blob)
		case "connect":
			// Browser code flow; only claude_code supports it, so it defaults.
			kind := "claude_code"
			if len(args) == 2 {
				kind = args[1]
			}
			return cli.RemoteAdminLLMOAuthConnect(cmd.Context(), c, p, kind)
		case "refresh":
			kind, err := needKind()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/llm/oauth/"+kind+"/refresh", nil)
		case "delete":
			kind, err := needKind()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/llm/oauth/"+kind, nil)
		default:
			return fmt.Errorf("unknown oauth action %q (want set|connect|refresh|delete)", action)
		}
	}),
}

// --- org SSO ---

var remoteSSOData string

var remoteSSOCmd = &cobra.Command{
	Use:   "sso",
	Short: "Per-org SSO providers and domains",
}

var remoteSSOProvidersCmd = &cobra.Command{
	Use:   "providers [create|update|delete|test <provider-id>]",
	Short: "Org SSO providers",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		base := "/api/orgs/" + org + "/sso/providers"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteSSOData, "provider JSON")
		case args[0] == "update" && len(args) == 2:
			return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", base+"/"+args[1], remoteSSOData, "patch JSON")
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		case args[0] == "test" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/test", nil)
		default:
			return fmt.Errorf("usage: providers [create|update|delete|test <provider-id>]")
		}
	}),
}

var remoteSSODomainsCmd = &cobra.Command{
	Use:   "domains [create|verify <domain-id>|delete <domain-id>]",
	Short: "Org SSO domains",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		base := "/api/orgs/" + org + "/sso/domains"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteSSOData, "domain JSON")
		case args[0] == "verify" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/verify", nil)
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: domains [create|verify <id>|delete <id>]")
		}
	}),
}

// --- plugins ---

var remotePluginsData string

var remotePluginsCmd = &cobra.Command{
	Use:   "plugins [enable|disable|install|uninstall|config <name>]",
	Short: "Plugin management on the remote instance",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/plugins")
		}
		action := args[0]
		switch action {
		case "install":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", "/api/v1/plugins/install", remotePluginsData, "install JSON")
		case "enable", "disable":
			if len(args) != 2 {
				return fmt.Errorf("usage: plugins %s <name>", action)
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/plugins/"+args[1]+"/"+action, nil)
		case "uninstall":
			if len(args) != 2 {
				return fmt.Errorf("usage: plugins uninstall <name>")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/v1/plugins/"+args[1], nil)
		case "config":
			if len(args) != 2 {
				return fmt.Errorf("usage: plugins config <name> --data @f")
			}
			return cli.RemoteSendData(cmd.Context(), c, p, "PUT", "/api/v1/plugins/"+args[1]+"/config", remotePluginsData, "config JSON")
		default:
			return fmt.Errorf("unknown plugins action %q", action)
		}
	}),
}

func init() {
	remoteAdminOrgsCmd.Flags().StringVar(&remoteAdminData, "data", "", "Request body JSON (literal or @file)")
	remoteAdminUsersCmd.Flags().StringVar(&remoteAdminData, "data", "", "Request body JSON (literal or @file)")

	for _, c := range []*cobra.Command{remoteAdminLLMKeysCmd, remoteAdminLLMOAuthCmd} {
		c.Flags().StringVar(&remoteLLMFromEnv, "from-env", "", "Read the secret value from this environment variable")
		c.Flags().StringVar(&remoteLLMFromFile, "from-file", "", "Read the secret value from this file")
	}
	remoteAdminLLMKeysCmd.Flags().StringVar(&remoteLLMProvider, "provider", "", "LLM provider for create (anthropic|openai|…)")
	remoteAdminLLMKeysCmd.Flags().StringVar(&remoteLLMName, "name", "", "Key display name for create")
	remoteAdminLLMKeysCmd.Flags().BoolVar(&remoteLLMDefault, "default", false, "Make the created key the provider's default")
	remoteAdminLLMKeysCmd.Flags().StringVar(&remoteLLMKeyData, "data", "", "Patch JSON for update (literal or @file)")
	remoteAdminLLMCmd.AddCommand(remoteAdminLLMKeysCmd, remoteAdminLLMOAuthCmd)

	remoteAdminCmd.AddCommand(remoteAdminOrgsCmd, remoteAdminUsersCmd, remoteAdminDLQCmd, remoteAdminLLMCmd)

	remoteSSOProvidersCmd.Flags().StringVar(&remoteSSOData, "data", "", "Request body JSON (literal or @file)")
	remoteSSODomainsCmd.Flags().StringVar(&remoteSSOData, "data", "", "Request body JSON (literal or @file)")
	remoteSSOProvidersCmd.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	remoteSSODomainsCmd.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	remoteSSOCmd.AddCommand(remoteSSOProvidersCmd, remoteSSODomainsCmd)

	remotePluginsCmd.Flags().StringVar(&remotePluginsData, "data", "", "Request body JSON (literal or @file)")

	remoteCmd.AddCommand(remoteAdminCmd, remoteSSOCmd, remotePluginsCmd)
}
