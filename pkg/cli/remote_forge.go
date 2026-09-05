package cli

import (
	"context"
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
	if _, err := c.Call(ctx, "POST", path, body, &res); err != nil {
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
