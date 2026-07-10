package main

import (
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote audit / usage / limits / memory — observability + governance.

var (
	remoteAuditSince string
	remoteAuditLimit int
)

func remoteAuditQuery() string {
	limit := ""
	if remoteAuditLimit > 0 {
		limit = strconv.Itoa(remoteAuditLimit)
	}
	return cli.QueryString(map[string]string{"since": remoteAuditSince, "limit": limit})
}

var remoteAuditCmd = &cobra.Command{
	Use:   "audit <team|org|admin>",
	Short: "Audit log (team-, org-, or platform-scoped)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		switch args[0] {
		case "team":
			base, err := teamBase(cmd, c, "/audit")
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+remoteAuditQuery())
		case "org":
			org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/orgs/"+org+"/audit"+remoteAuditQuery())
		case "admin":
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/audit"+remoteAuditQuery())
		default:
			return fmt.Errorf("unknown audit scope %q (want team|org|admin)", args[0])
		}
	}),
}

var remoteUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Org monthly usage (alias of `orgs usage`)",
	Args:  cobra.NoArgs,
	// True alias: same body as `orgs usage`.
	RunE: func(cmd *cobra.Command, args []string) error {
		return remoteOrgsUsageCmd.RunE(cmd, args)
	},
}

var remoteLimitsData string

var remoteLimitsCmd = &cobra.Command{
	Use:   "limits [cost|override]",
	Short: "Cost limits: show (cost) or set an override (--data)",
	Args:  cobra.MaximumNArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		action := "cost"
		if len(args) == 1 {
			action = args[0]
		}
		switch action {
		case "cost":
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/limits/cost")
		case "override":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", "/api/v1/limits/cost/override", remoteLimitsData, "override JSON")
		default:
			return fmt.Errorf("unknown limits action %q (want cost|override)", action)
		}
	}),
}

// --- shared workspace memory ---

var remoteMemoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Shared workspace memory on the remote instance",
}

var remoteMemoryUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Memory store usage",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/memory/usage")
	}),
}

var (
	remoteMemoryName       string
	remoteMemoryVisibility string
	remoteMemoryBot        string
	remoteMemoryProject    string
)

// remoteMemoryQuery builds the SpaceRef query the memory endpoints
// resolve (name required; visibility defaults server-side to project).
func remoteMemoryQuery(extra map[string]string) (string, error) {
	if remoteMemoryName == "" {
		return "", fmt.Errorf("--name is required (memory space name)")
	}
	pairs := map[string]string{
		"name":       remoteMemoryName,
		"visibility": remoteMemoryVisibility,
		"bot":        remoteMemoryBot,
		"project":    remoteMemoryProject,
	}
	for k, v := range extra {
		pairs[k] = v
	}
	return cli.QueryString(pairs), nil
}

var remoteMemoryDir string

var remoteMemoryDocsCmd = &cobra.Command{
	Use:   "docs",
	Short: "List memory documents in a space (--name)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		q, err := remoteMemoryQuery(map[string]string{"dir": remoteMemoryDir})
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/memory/docs"+q)
	}),
}

var remoteMemoryDocData string

var remoteMemoryDocCmd = &cobra.Command{
	Use:   "doc <get|put|delete> <path>",
	Short: "Read, write or delete one memory document",
	Args:  cobra.ExactArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		q, err := remoteMemoryQuery(map[string]string{"path": args[1]})
		if err != nil {
			return err
		}
		switch args[0] {
		case "get":
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/memory/doc"+q)
		case "put":
			return cli.RemoteSendData(cmd.Context(), c, p, "PUT", "/api/memory/doc"+q, remoteMemoryDocData, "document content")
		case "delete":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/memory/doc"+q, nil)
		default:
			return fmt.Errorf("unknown doc action %q (want get|put|delete)", args[0])
		}
	}),
}

var remoteMemoryExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export the memory tree",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		q, err := remoteMemoryQuery(nil)
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/memory/export"+q)
	}),
}

var (
	remoteMemoryImportData string
	remoteMemoryStrategy   string
)

var remoteMemoryImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import a memory export (--data @file)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		q, err := remoteMemoryQuery(map[string]string{"strategy": remoteMemoryStrategy})
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "POST", "/api/memory/import"+q, remoteMemoryImportData, "export payload")
	}),
}

func init() {
	remoteAuditCmd.Flags().StringVar(&remoteAuditSince, "since", "", "Only entries after (RFC3339)")
	remoteAuditCmd.Flags().IntVar(&remoteAuditLimit, "limit", 0, "Max entries")
	remoteAuditCmd.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (team scope)")
	remoteAuditCmd.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (org scope)")
	remoteUsageCmd.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	remoteLimitsCmd.Flags().StringVar(&remoteLimitsData, "data", "", "Override JSON (literal or @file)")

	for _, c := range []*cobra.Command{remoteMemoryDocsCmd, remoteMemoryDocCmd, remoteMemoryExportCmd, remoteMemoryImportCmd} {
		c.Flags().StringVar(&remoteMemoryName, "name", "", "Memory space name (required)")
		c.Flags().StringVar(&remoteMemoryVisibility, "visibility", "", "Space visibility (project|bot|user|team)")
		c.Flags().StringVar(&remoteMemoryBot, "bot", "", "Bot id (visibility bot)")
		c.Flags().StringVar(&remoteMemoryProject, "project", "", "Project key")
	}
	remoteMemoryDocsCmd.Flags().StringVar(&remoteMemoryDir, "dir", "", "Subdirectory to list")
	remoteMemoryDocCmd.Flags().StringVar(&remoteMemoryDocData, "data", "", "Document content (literal or @file)")
	remoteMemoryImportCmd.Flags().StringVar(&remoteMemoryImportData, "data", "", "Export payload (@file)")
	remoteMemoryImportCmd.Flags().StringVar(&remoteMemoryStrategy, "strategy", "", "Import strategy")
	remoteMemoryCmd.AddCommand(remoteMemoryUsageCmd, remoteMemoryDocsCmd, remoteMemoryDocCmd, remoteMemoryExportCmd, remoteMemoryImportCmd)

	remoteCmd.AddCommand(remoteAuditCmd, remoteUsageCmd, remoteLimitsCmd, remoteMemoryCmd)
}
