package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// RemoteCredentialsPreview asks the server for an observation of the launch
// chain. Only the server knows the source owner, pins, audience and quotas.
// This command neither changes the active team nor launches a run.
func RemoteCredentialsPreview(ctx context.Context, c *RemoteClient, p *Printer, team, bot, webhook string) error {
	bot, webhook = strings.TrimSpace(bot), strings.TrimSpace(webhook)
	if bot == "" && webhook == "" {
		return fmt.Errorf("--bot is required for a personal launch preview")
	}
	id, err := c.ResolveTeam(ctx, team)
	if err != nil {
		return err
	}
	request := struct {
		Source struct {
			Kind string `json:"kind"`
			ID   string `json:"id,omitempty"`
		} `json:"source"`
		BotID string `json:"bot_id,omitempty"`
	}{BotID: bot}
	request.Source.Kind = "personal"
	if webhook != "" {
		request.Source.Kind, request.Source.ID = "webhook", webhook
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return RemoteSendPrint(ctx, c, p, "POST", "/api/teams/"+url.PathEscape(id)+"/credentials/preview", body)
}
