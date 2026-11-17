package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var remoteCredentialsCmd = &cobra.Command{
	Use:   "credentials",
	Short: "Inspect the credentials that could fund a launch",
}

var remoteCredentialsBot, remoteCredentialsWebhook, remoteCredentialsReviewMode string

var remoteCredentialsPreviewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Preview the ordered credential chain and observed fallback conditions",
	Long: "Observe the server's credential chain for a personal bot launch or an existing\n" +
		"webhook. The server derives the real owner, permissions and key pins. This\n" +
		"read-only preview does not reserve capacity or verify secrets with providers.\n" +
		"It prints the observation as JSON; a later launch can encounter changed state.",
	Args: cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, _ []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteCredentialsPreview(cmd.Context(), c, p, remoteTeamFlag, remoteCredentialsBot, remoteCredentialsWebhook, remoteCredentialsReviewMode)
	}),
}

func init() {
	remoteCredentialsPreviewCmd.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")
	remoteCredentialsPreviewCmd.Flags().StringVar(&remoteCredentialsBot, "bot", "", "Catalog bot id (required for a personal launch)")
	remoteCredentialsPreviewCmd.Flags().StringVar(&remoteCredentialsWebhook, "webhook", "", "Preview this team's existing webhook and its real launch context")
	remoteCredentialsPreviewCmd.Flags().StringVar(&remoteCredentialsReviewMode, "review-mode", "", "Run-level review topology (mono|dual) the preview answers for — a mono pin is a credential demand")
	remoteCredentialsCmd.AddCommand(remoteCredentialsPreviewCmd)
	remoteCmd.AddCommand(remoteCredentialsCmd)
}
