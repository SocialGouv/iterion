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
