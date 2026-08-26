package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server"
)

// --- Personal access tokens (/api/me/tokens) ---

type remotePAT struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	TokenLast4  string     `json:"token_last4"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	TeamID      string     `json:"team_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

func RemoteTokensList(ctx context.Context, c *RemoteClient, p *Printer) error {
	if p.Format == OutputJSON {
		return RemoteGetPrint(ctx, c, p, "/api/me/tokens")
	}
	var out struct {
		Tokens []remotePAT `json:"tokens"`
	}
	if _, err := c.Call(ctx, "GET", "/api/me/tokens", nil, &out); err != nil {
		return err
	}
	rows := make([][]string, 0, len(out.Tokens))
	for _, t := range out.Tokens {
		state := "active"
		if t.RevokedAt != nil {
			state = "revoked"
		} else if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
			state = "expired"
		}
		expires := "—"
		if t.ExpiresAt != nil {
			expires = FormatTime(*t.ExpiresAt)
		}
		rows = append(rows, []string{t.ID, t.Name, "…" + t.TokenLast4, t.TeamID, state, expires})
	}
	p.Table([]string{"TOKEN ID", "NAME", "LAST4", "TEAM PIN", "STATE", "EXPIRES"}, rows)
	return nil
}

// RemoteTokensCreate mints a PAT and prints the plaintext once.
func RemoteTokensCreate(ctx context.Context, c *RemoteClient, p *Printer, name, teamID string, expiresInDays int) error {
	req := map[string]any{"name": name, "expires_in_days": expiresInDays}
	if teamID != "" {
		req["team_id"] = teamID
	}
	var out struct {
		PAT   remotePAT `json:"pat"`
		Token string    `json:"token"`
	}
	raw, err := c.Call(ctx, "POST", "/api/me/tokens", req, &out)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		PrintRemoteJSON(p, raw)
		return nil
	}
	p.Line("Token %s created", out.PAT.ID)
	p.Line("%s", out.Token)
	p.Line("(store it now — the plaintext is never shown again)")
	return nil
}

func RemoteTokensRevoke(ctx context.Context, c *RemoteClient, p *Printer, tokenID string) error {
	if _, err := c.Call(ctx, "DELETE", "/api/me/tokens/"+tokenID, nil, nil); err != nil {
		return err
	}
	p.Line("Token %s revoked", tokenID)
	return nil
}

// --- teams / orgs ---

func RemoteTeamsList(ctx context.Context, c *RemoteClient, p *Printer) error {
	if p.Format == OutputJSON {
		return RemoteGetPrint(ctx, c, p, "/api/teams")
	}
	// Decode with the server's own membership view type (see RemoteMe):
	// hand-mirrored tags here once rendered an all-empty table with every
	// row starred as active.
	var out struct {
		Teams []server.MembershipView `json:"teams"`
	}
	if _, err := c.Call(ctx, "GET", "/api/teams", nil, &out); err != nil {
		return err
	}
	active := c.cfg.TeamID
	rows := make([][]string, 0, len(out.Teams))
	for _, t := range out.Teams {
		mark := ""
		if t.TeamID != "" && t.TeamID == active {
			mark = "*"
		}
		name := t.TeamName
		if t.Personal {
			name += " (personal)"
		}
		rows = append(rows, []string{mark, t.TeamID, name, t.Role})
	}
	p.Table([]string{"", "TEAM ID", "NAME", "ROLE"}, rows)
	return nil
}

// RemoteTeamsSwitch changes the CLI's default team. A PAT's identity
// team is pinned at mint time, so switching means minting a NEW token
// pinned to the target team, persisting it, and revoking the previous
// CLI token (identified deterministically by its SHA-256 fingerprint;
// an env-provided or unmatched token is left alone with a notice).
func RemoteTeamsSwitch(ctx context.Context, c *RemoteClient, p *Printer, teamID, tokenName string) error {
	if strings.TrimSpace(teamID) == "" {
		return fmt.Errorf("team id required (see `iterion remote teams list`)")
	}
	if os.Getenv("ITERION_REMOTE_URL") != "" {
		return fmt.Errorf("teams switch mutates the stored credential and cannot run in ITERION_REMOTE_URL env mode — mint a team-pinned token instead: `iterion remote tokens create --team %s`", teamID)
	}
	// No client-side membership pre-check: the mint endpoint's canViewTeam
	// is the authority (it also grants org-admins and super-admins teams a
	// plain membership listing may not carry). The server states the
	// team’s org in the mint response — it owns that fact.
	oldFingerprint := secrets.FingerprintSHA256(c.cfg.Token)

	var minted struct {
		PAT   remotePAT `json:"pat"`
		Token string    `json:"token"`
		OrgID string    `json:"org_id"`
	}
	req := map[string]any{"name": tokenName, "team_id": teamID, "expires_in_days": 0}
	if _, err := c.Call(ctx, "POST", "/api/me/tokens", req, &minted); err != nil {
		return fmt.Errorf("%w (see `iterion remote teams list`)", err)
	}
	if minted.Token == "" {
		return fmt.Errorf("token endpoint returned no token")
	}

	cfg := c.cfg
	cfg.Token = minted.Token
	cfg.TeamID = teamID
	// Follow the team into its org so org-scoped commands aim where the
	// new token lives (empty from a pre-org_id server: scope unchanged).
	if minted.OrgID != "" {
		cfg.OrgID = minted.OrgID
	}
	if err := SaveRemoteConfig(cfg); err != nil {
		return err
	}
	p.Line("Switched to team %s (new CLI token %s)", teamID, minted.PAT.ID)

	// Revoke the replaced CLI token — matched by fingerprint so we never
	// revoke a token the operator minted for something else.
	newClient := NewRemoteClientFor(cfg)
	var toks struct {
		Tokens []remotePAT `json:"tokens"`
	}
	if _, err := newClient.Call(ctx, "GET", "/api/me/tokens", nil, &toks); err != nil {
		return fmt.Errorf("switched, but could not list tokens to revoke the previous one: %w", err)
	}
	for _, t := range toks.Tokens {
		if t.Fingerprint != "" && t.Fingerprint == oldFingerprint && t.RevokedAt == nil {
			if _, err := newClient.Call(ctx, "DELETE", "/api/me/tokens/"+t.ID, nil, nil); err != nil {
				return fmt.Errorf("switched, but revoking the previous CLI token %s failed: %w", t.ID, err)
			}
			p.Line("Previous CLI token %s revoked", t.ID)
			return nil
		}
	}
	p.Line("Previous token not found among your tokens (env-provided or already revoked) — left untouched")
	return nil
}

// RemoteOrgsSwitch persists the default org used by org-scoped commands.
// Org scope is path-based for the API, so no re-mint is needed.
func RemoteOrgsSwitch(ctx context.Context, c *RemoteClient, p *Printer, orgID string) error {
	if os.Getenv("ITERION_REMOTE_URL") != "" {
		return fmt.Errorf("orgs switch mutates the stored credential and cannot run in ITERION_REMOTE_URL env mode — set ITERION_REMOTE_ORG instead")
	}
	me, err := c.Me(ctx)
	if err != nil {
		return err
	}
	found := me.ActiveOrg == orgID
	for _, o := range me.Orgs {
		if o.OrgID == orgID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no membership found in org %q", orgID)
	}
	cfg := c.cfg
	cfg.OrgID = orgID
	if err := SaveRemoteConfig(cfg); err != nil {
		return err
	}
	p.Line("Default org set to %s", orgID)
	return nil
}

// RemoteOrgsList renders the orgs derivable from the account's memberships.
func RemoteOrgsList(ctx context.Context, c *RemoteClient, p *Printer) error {
	me, err := c.Me(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	type orgRow struct {
		ID     string `json:"id"`
		Name   string `json:"name,omitempty"`
		Role   string `json:"role,omitempty"`
		Active bool   `json:"active"`
	}
	var orgs []orgRow
	add := func(id, name, role string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		orgs = append(orgs, orgRow{ID: id, Name: name, Role: role, Active: id == me.ActiveOrg})
	}
	for _, o := range me.Orgs {
		add(o.OrgID, o.OrgName, o.OrgRole)
	}
	// The active org is normally in the tree; keep it visible even if not
	// (e.g. an org the account was removed from while its token lives on).
	add(me.ActiveOrg, "", "")
	if p.Format == OutputJSON {
		p.JSON(map[string]any{"orgs": orgs})
		return nil
	}
	rows := make([][]string, 0, len(orgs))
	for _, o := range orgs {
		mark := ""
		if o.Active {
			mark = "*"
		}
		rows = append(rows, []string{mark, o.ID, o.Name, o.Role})
	}
	p.Table([]string{"", "ORG ID", "NAME", "ROLE"}, rows)
	return nil
}
