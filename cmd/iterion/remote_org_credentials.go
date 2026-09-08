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

func init() {
	for _, c := range []*cobra.Command{remoteOrgsOAuthCmd, remoteOrgsAudienceCmd} {
		c.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	}
	remoteOrgsOAuthCmd.Flags().StringVar(&remoteSecretFromEnv, "from-env", "", "Read the credentials blob from this environment variable")
	remoteOrgsOAuthCmd.Flags().StringVar(&remoteSecretFromFile, "from-file", "", "Read the credentials blob from this file")
	remoteOrgsAudienceCmd.Flags().StringVar(&remoteAudienceTeams, "teams", "", "Comma-separated team ids allowed to spend the org's credentials (empty string revokes all)")
	remoteOrgsAudienceCmd.Flags().StringVar(&remoteAudienceAllTeams, "all-teams", "", "true|false — admit every team of the org")

	remoteOrgsCmd.AddCommand(remoteOrgsOAuthCmd, remoteOrgsAudienceCmd)
}
