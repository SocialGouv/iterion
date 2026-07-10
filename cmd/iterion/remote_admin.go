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
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		p := newPrinter()
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
			body, err := cli.ReadDataArg(remoteAdminData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required (org JSON)")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/orgs", body)
		case "update":
			id, err := needID()
			if err != nil {
				return err
			}
			body, err := cli.ReadDataArg(remoteAdminData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required (patch JSON)")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "PATCH", "/api/admin/orgs/"+id, body)
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
	},
}

var remoteAdminUsersCmd = &cobra.Command{
	Use:   "users [update <user-id>]",
	Short: "List platform users, or update one (--data)",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		p := newPrinter()
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/users")
		}
		if args[0] != "update" || len(args) != 2 {
			return fmt.Errorf("usage: admin users [update <user-id> --data @f]")
		}
		body, err := cli.ReadDataArg(remoteAdminData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required (patch JSON)")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "PATCH", "/api/admin/users/"+args[1], body)
	},
}

var remoteAdminDLQCmd = &cobra.Command{
	Use:   "dlq [show|replay|delete <seq>]",
	Short: "Queue dead-letter entries: list (default) or act on one",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		p := newPrinter()
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
	},
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
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/orgs/" + org + "/sso/providers"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			body, err := cli.ReadDataArg(remoteSSOData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required (provider JSON)")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, body)
		case args[0] == "update" && len(args) == 2:
			body, err := cli.ReadDataArg(remoteSSOData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required (patch JSON)")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "PATCH", base+"/"+args[1], body)
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		case args[0] == "test" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/test", nil)
		default:
			return fmt.Errorf("usage: providers [create|update|delete|test <provider-id>]")
		}
	},
}

var remoteSSODomainsCmd = &cobra.Command{
	Use:   "domains [create|verify <domain-id>|delete <domain-id>]",
	Short: "Org SSO domains",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/orgs/" + org + "/sso/domains"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			body, err := cli.ReadDataArg(remoteSSOData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required (domain JSON)")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, body)
		case args[0] == "verify" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/verify", nil)
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: domains [create|verify <id>|delete <id>]")
		}
	},
}

// --- plugins ---

var remotePluginsData string

var remotePluginsCmd = &cobra.Command{
	Use:   "plugins [enable|disable|install|uninstall|config <name>]",
	Short: "Plugin management on the remote instance",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		p := newPrinter()
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/plugins")
		}
		action := args[0]
		switch action {
		case "install":
			body, err := cli.ReadDataArg(remotePluginsData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required (install JSON)")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/plugins/install", body)
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
			body, err := cli.ReadDataArg(remotePluginsData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required (config JSON)")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", "/api/v1/plugins/"+args[1]+"/config", body)
		default:
			return fmt.Errorf("unknown plugins action %q", action)
		}
	},
}

func init() {
	remoteAdminOrgsCmd.Flags().StringVar(&remoteAdminData, "data", "", "Request body JSON (literal or @file)")
	remoteAdminUsersCmd.Flags().StringVar(&remoteAdminData, "data", "", "Request body JSON (literal or @file)")
	remoteAdminCmd.AddCommand(remoteAdminOrgsCmd, remoteAdminUsersCmd, remoteAdminDLQCmd)

	remoteSSOProvidersCmd.Flags().StringVar(&remoteSSOData, "data", "", "Request body JSON (literal or @file)")
	remoteSSODomainsCmd.Flags().StringVar(&remoteSSOData, "data", "", "Request body JSON (literal or @file)")
	remoteSSOProvidersCmd.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	remoteSSODomainsCmd.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	remoteSSOCmd.AddCommand(remoteSSOProvidersCmd, remoteSSODomainsCmd)

	remotePluginsCmd.Flags().StringVar(&remotePluginsData, "data", "", "Request body JSON (literal or @file)")

	remoteCmd.AddCommand(remoteAdminCmd, remoteSSOCmd, remotePluginsCmd)
}
