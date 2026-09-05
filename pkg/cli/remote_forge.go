package cli

import (
	"context"
	"encoding/json"
	"sort"
)

type remoteForgeRefreshResult struct {
	InstallationAccount     string            `json:"installation_account"`
	ManageInstallURL        string            `json:"manage_install_url"`
	GrantedPermissions      map[string]string `json:"granted_permissions"`
	TokenPermissions        map[string]string `json:"token_permissions"`
	MissingPermissions      []string          `json:"missing_permissions"`
	TokenMissingPermissions []string          `json:"token_missing_permissions"`
	LiveError               string            `json:"live_error"`
}

// RemoteForgeRefresh POSTs to a connection's /refresh endpoint, which re-probes
// the GitHub-App installation and re-syncs the stored granted permissions
// immediately — so a just-changed App permission (e.g. Commit statuses: write
// for the merge gate) is picked up without waiting for the periodic refresh
// worker or restarting the server. Prints what the installation GRANTS vs what
// the minted TOKEN carries (they differ, and the token is what acts).
func RemoteForgeRefresh(ctx context.Context, c *RemoteClient, p *Printer, path string) error {
	var res remoteForgeRefreshResult
	if _, err := c.Call(ctx, "POST", path, nil, &res); err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(res)
		return nil
	}
	p.Header("Forge connection refreshed")
	if res.InstallationAccount != "" {
		p.KV("installation", res.InstallationAccount)
	}
	keys := map[string]bool{}
	for k := range res.GrantedPermissions {
		keys[k] = true
	}
	for k := range res.TokenPermissions {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	rows := make([][]string, 0, len(ordered))
	for _, k := range ordered {
		rows = append(rows, []string{k, res.GrantedPermissions[k], res.TokenPermissions[k]})
	}
	p.Table([]string{"PERMISSION", "GRANTED", "TOKEN"}, rows)
	if res.LiveError != "" {
		p.Line("live probe error: %s", res.LiveError)
	}
	return nil
}

type remoteForgeAvatarResult struct {
	Connection struct {
		ID              string `json:"id"`
		Provider        string `json:"provider"`
		AccountLogin    string `json:"account_login"`
		AvatarAppliedAt string `json:"avatar_applied_at"`
	} `json:"connection"`
	AvatarURL string `json:"avatar_url"`
}

// RemoteForgeAvatar POSTs to a connection's /avatar endpoint — upload the
// iterion-bot avatar onto the account behind it — and prints the outcome. A
// refusal (a person's OAuth account, a GitHub connection, an account the forge
// does not flag as a bot without --force) comes back as the API error, whose
// message names the alternative.
func RemoteForgeAvatar(ctx context.Context, c *RemoteClient, p *Printer, path, variant string, force bool) error {
	body := map[string]any{"force": force}
	if variant != "" {
		body["variant"] = variant
	}
	var res remoteForgeAvatarResult
	raw, err := c.Call(ctx, "POST", path, body, &res)
	if err != nil {
		// The generic error line truncates the JSON body, and the field that
		// matters on a refusal — where to upload by hand — sits last in it.
		printAvatarRefusal(p, raw)
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(res)
		return nil
	}
	p.Header("iterion-bot avatar applied")
	p.KV("connection", res.Connection.ID)
	p.KV("account", "@"+res.Connection.AccountLogin+" ("+res.Connection.Provider+")")
	if res.AvatarURL != "" {
		p.KV("avatar_url", res.AvatarURL)
	}
	return nil
}

// printAvatarRefusal renders the refusal fields of the avatar endpoint (422 on
// GitHub with the App's settings page, 409 when the account is not flagged
// as a bot) so the operator reads the alternative, not a cut JSON blob.
func printAvatarRefusal(p *Printer, raw []byte) {
	var refusal struct {
		Error        string `json:"error"`
		ManageURL    string `json:"manage_url"`
		LogoURL      string `json:"logo_url"`
		NeedsForce   bool   `json:"needs_force"`
		AccountLogin string `json:"account_login"`
	}
	if json.Unmarshal(raw, &refusal) != nil || refusal.Error == "" {
		return
	}
	if p.Format == OutputJSON {
		p.JSON(json.RawMessage(raw))
		return
	}
	p.Header("iterion-bot avatar not applied")
	p.KV("reason", refusal.Error)
	if refusal.ManageURL != "" {
		p.KV("upload it here", refusal.ManageURL)
	}
	if refusal.LogoURL != "" {
		p.KV("logo", refusal.LogoURL)
	}
	if refusal.NeedsForce {
		p.KV("retry with", "--force (only for a dedicated account, never a person's)")
	}
}
