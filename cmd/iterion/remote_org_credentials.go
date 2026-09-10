package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote orgs credentials — the org tier's control surface: the shared
// forfait connections, and the AUDIENCE naming which of the org's teams may
// spend the org's keys. The api-key half rides `remote api-keys --scope org`
// (one authority for every scope; see cli.RemoteSecretsBase).
//
// The audience's zero value admits nobody, so an org that never ran these
// commands behaves exactly as before.

var remoteOrgsOAuthCmd = &cobra.Command{
	Use:   "oauth [set|refresh|delete <kind>]",
	Short: "The org's shared OAuth forfait connections (claude_code|codex)",
	Long: "List the org's shared forfait connections, or set one from a credentials\n" +
		"blob (--from-file/--from-env/stdin). The org tier lends these to the teams\n" +
		"named by `orgs credential-audience`.",
	Args: cobra.RangeArgs(0, 2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		base := "/api/orgs/" + org + "/oauth"
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/connections")
		}
		if len(args) != 2 {
			return fmt.Errorf("usage: orgs oauth [set|refresh|delete <kind>]")
		}
		action, kind := args[0], args[1]
		switch action {
		case "set":
			// The blob is read whole and ANSI-stripped, like every other
			// credential ingestion path: a credentials.json copied out of a
			// pager otherwise fails server-side on "\x1b", accurately and
			// uselessly.
			blob, err := cli.ReadCredentialBlob(remoteSecretFromEnv, remoteSecretFromFile, kind)
			if err != nil {
				return err
			}
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base+"/"+kind+"/credentials", string(blob), "credentials JSON")
		case "refresh":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+kind+"/refresh", nil)
		case "delete":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+kind, nil)
		default:
			return fmt.Errorf("unknown action %q (want set|refresh|delete)", action)
		}
	}),
}

var (
	remoteAudienceTeams    string
	remoteAudienceAllTeams string
)

var remoteOrgsAudienceCmd = &cobra.Command{
	Use:   "credential-audience",
	Short: "Which of the org's teams may spend the org's shared LLM credentials",
	Long: "Show the audience, or set it with --teams / --all-teams.\n\n" +
		"The zero value admits NOBODY: lending a credential is an explicit act,\n" +
		"so a team that was never named funds its own runs or does not run.\n" +
		"A team of another org is refused — this is an authorization list.",
	Args: cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		path := "/api/orgs/" + org + "/credential-audience"
		if remoteAudienceTeams == "" && remoteAudienceAllTeams == "" {
			return cli.RemoteGetPrint(cmd.Context(), c, p, path)
		}
		body := map[string]any{}
		if cmd.Flags().Changed("teams") {
			// An explicit empty --teams "" is how an operator REVOKES every
			// named team, so it must send [] rather than be read as "unset".
			teams := []string{}
			for _, t := range strings.Split(remoteAudienceTeams, ",") {
				if t = strings.TrimSpace(t); t != "" {
					teams = append(teams, t)
				}
			}
			body["teams"] = teams
		}
		if cmd.Flags().Changed("all-teams") {
			switch remoteAudienceAllTeams {
			case "true", "on", "yes":
				body["all_teams"] = true
			case "false", "off", "no":
				body["all_teams"] = false
			default:
				return fmt.Errorf("--all-teams wants true|false, got %q", remoteAudienceAllTeams)
			}
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", path, string(raw), "audience JSON")
	}),
}

var remoteOrgMemberRole string

var remoteOrgsAddMemberCmd = &cobra.Command{
	Use:   "add-member <user-id>",
	Short: "Place an EXISTING account in the org with an org role (--role)",
	Long: "The org-level twin of `teams add-member`, and the half without which\n" +
		"that one cannot serve the case it exists for: a user with no org at all\n" +
		"is otherwise reachable only by email. Idempotent — re-running sets the\n" +
		"role. For an account that does not exist yet, use `orgs invitations`.",
	Args: cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remoteOrgMemberRole == "" {
			return fmt.Errorf("--role is required (member|admin|owner)")
		}
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(map[string]any{"role": remoteOrgMemberRole})
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PUT", "/api/orgs/"+org+"/members/"+args[0], string(raw), "member JSON")
	}),
}

// --- governance: settings + the provisioning approval queue ---

var (
	remoteOrgApprovalRequire string
	remoteOrgApprovalScope   string
)

var remoteOrgsSettingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "Org governance settings (provisioning approval)",
	Long: "Show the org's governance settings, or set them.\n\n" +
		"--require-approval parks a TEAM admin's repo-bot provisioning until an\n" +
		"org admin approves; nothing is created forge-side meanwhile.\n" +
		"--approval-scope narrows what it parks: `all` (every request) or\n" +
		"`shared_credentials` (only teams with no credential of their own —\n" +
		"a team spending its own BYOK answers to nobody for what it runs).",
	Args: cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		path := "/api/orgs/" + org + "/settings"
		if !cmd.Flags().Changed("require-approval") && !cmd.Flags().Changed("approval-scope") {
			return cli.RemoteGetPrint(cmd.Context(), c, p, path)
		}
		body := map[string]any{}
		if cmd.Flags().Changed("require-approval") {
			switch remoteOrgApprovalRequire {
			case "true", "on", "yes":
				body["require_provision_approval"] = true
			case "false", "off", "no":
				body["require_provision_approval"] = false
			default:
				return fmt.Errorf("--require-approval wants true|false, got %q", remoteOrgApprovalRequire)
			}
		}
		if cmd.Flags().Changed("approval-scope") {
			body["provision_approval_scope"] = remoteOrgApprovalScope
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", path, string(raw), "settings JSON")
	}),
}

var remoteOrgsApprovalsCmd = &cobra.Command{
	Use:   "approvals [approve|reject <approval-id>]",
	Short: "The pending repo-bot provisioning requests of the org",
	Long: "List what team admins asked for and nothing created yet, or decide.\n" +
		"Approving REPLAYS the exact recorded request through the orchestrator;\n" +
		"a request whose target moved since is refused (409) so a stale record\n" +
		"is rejected rather than re-provisioned by surprise.",
	Args: cobra.RangeArgs(0, 2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		base := "/api/orgs/" + org + "/provision-approvals"
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		}
		if len(args) != 2 {
			return fmt.Errorf("usage: orgs approvals [approve|reject <approval-id>]")
		}
		switch args[0] {
		case "approve", "reject":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/"+args[0], nil)
		default:
			return fmt.Errorf("unknown action %q (want approve|reject)", args[0])
		}
	}),
}

func init() {
	for _, c := range []*cobra.Command{
		remoteOrgsOAuthCmd, remoteOrgsAudienceCmd, remoteOrgsSettingsCmd, remoteOrgsApprovalsCmd,
		remoteOrgsAddMemberCmd,
	} {
		c.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	}
	remoteOrgsSettingsCmd.Flags().StringVar(&remoteOrgApprovalRequire, "require-approval", "", "true|false — park a team admin's repo provisioning for an org admin")
	remoteOrgsSettingsCmd.Flags().StringVar(&remoteOrgApprovalScope, "approval-scope", "", "all|shared_credentials — what the approval gate parks")
	remoteOrgsOAuthCmd.Flags().StringVar(&remoteSecretFromEnv, "from-env", "", "Read the credentials blob from this environment variable")
	remoteOrgsOAuthCmd.Flags().StringVar(&remoteSecretFromFile, "from-file", "", "Read the credentials blob from this file")
	remoteOrgsAudienceCmd.Flags().StringVar(&remoteAudienceTeams, "teams", "", "Comma-separated team ids allowed to spend the org's credentials (empty string revokes all)")
	remoteOrgsAudienceCmd.Flags().StringVar(&remoteAudienceAllTeams, "all-teams", "", "true|false — admit every team of the org")

	remoteOrgsAddMemberCmd.Flags().StringVar(&remoteOrgMemberRole, "role", "", "Org role (member|admin|owner)")

	remoteOrgsCmd.AddCommand(remoteOrgsOAuthCmd, remoteOrgsAudienceCmd, remoteOrgsSettingsCmd, remoteOrgsApprovalsCmd, remoteOrgsAddMemberCmd)
}
