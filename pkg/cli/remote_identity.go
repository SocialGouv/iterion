package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server"
)

// --- what a personal access token may not do ---

// A personal access token authenticates; it does not DERIVE another
// credential. The instance refuses it at every route that mints one — POST
// /api/me/tokens and the team/org switches — because a PAT is pinned to its
// team and its own expiry, and a derived credential would escape both.
//
// The CLI's stored credential IS a personal access token (RemoteConfig.Token,
// `iap_…`), so the two commands that mint say so before presenting it: the
// request would come back 403 and read as a server fault, and the operator
// would learn nothing of the path that does work — a login.
const patCannotMint = "a personal access token cannot mint another credential: the instance refuses it (403)"

// patCannotMintSentence is the server's own wording for that refusal
// (pkg/auth/derive.go), matched to recognise a 403 that reached a command
// whose local check could not see it coming — a credential that is not
// `iap_`-prefixed: an access token pasted into ITERION_REMOTE_TOKEN, a token
// minted before the prefix.
const patCannotMintSentence = "cannot mint another credential"

// presentsPAT reports whether what this client sends on its own is a personal
// access token — stored in ~/.iterion/cli-auth.json, or taken from the
// environment (ITERION_REMOTE_TOKEN / ITERION_TOKEN): ResolveRemoteConfig has
// already chosen between them.
func (c *RemoteClient) presentsPAT() bool {
	return strings.HasPrefix(strings.TrimSpace(c.cfg.Token), pat.TokenPrefix)
}

// refuseIfPAT is the pre-flight refusal, advice included, of a command that
// would mint a credential from the stored one. Nothing is sent.
func (c *RemoteClient) refuseIfPAT(advice string) error {
	if !c.presentsPAT() {
		return nil
	}
	return fmt.Errorf("%s\n%s", patCannotMint, advice)
}

// asPATMintRefusal reports whether err is the instance's own refusal to
// derive a credential, and renders it with the same advice — for the
// credential the local check could not recognise.
func asPATMintRefusal(err error, advice string) (error, bool) {
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusForbidden && strings.Contains(ae.Body, patCannotMintSentence) {
		return fmt.Errorf("%s\n%s", describeErrorBody([]byte(ae.Body)), advice), true
	}
	return err, false
}

// RefuseTeamTokenMintFromPAT is the pre-flight refusal every command whose
// route MINTS A TEAM TOKEN puts first (auth.MintTeamToken — the webhook
// mints, the share-link mints, the team and org invitations, and the
// repo-bot provisioning whose orchestrator mints the repo's hook token).
// The instance refuses every one of them to a personal access token (403,
// #1668), and a personal access token is the only thing the CLI ever stores
// — every `remote login` branch saves an `iap_…` — so the refusal is stated
// here, before anything is sent, with the one place the deed can be done.
//
// Its CLI callers are `webhooks create`, `webhooks rotate`, `teams|orgs
// invitations create`, `forge repo-bots create|update` and `orgs approvals
// approve`; the share-link mints have no first-hand command.
//
// where names that place in the studio ("Team → Webhooks"), because naming
// a login would be a dead end: the login would store another personal
// access token, which mints no more than this one.
func (c *RemoteClient) RefuseTeamTokenMintFromPAT(where string) error {
	return c.refuseIfPAT(teamTokenMintAdvice(c.BaseURL(), where))
}

// teamTokenMintAdvice is that refusal's advice, in one place so every one of
// those commands says the same thing.
func teamTokenMintAdvice(base, where string) string {
	return "Do it in the studio: " + strings.TrimRight(base, "/") + " → " + where + ".\n" +
		"The CLI stores a personal access token, which is pinned to its team and its own expiry;\n" +
		"a token that would outlive it — a webhook, share-link or invitation token — is minted by a live session only."
}

// teamSwitchAdvice names the two ways to put the CLI on another team, given
// that only a sign-in mints a team-pinned token.
func teamSwitchAdvice(base, teamID string) string {
	return "Log in again pinned to the team:\n" +
		"  iterion remote login " + base + " --email <you> --team " + teamID + "\n" +
		"or switch to the team in the studio and run `iterion remote login " + base +
		"` (the browser approval pins the studio's active team)."
}

// TeamApprovalResult is what a browser approval handed back. Every field is
// a CLAIM of the page, verified against the server before anything is
// stored (RemoteTeamsSwitch): the approval runs in a browser whose live
// session the CLI does not control.
type TeamApprovalResult struct {
	// Token is the minted plaintext.
	Token string
	// TeamID is the team the page states it pinned the token to — empty
	// from a page that does not know ?team= at all, which is how an
	// instance older than the CLI announces itself.
	TeamID string
	// OrgID is the org the page states for that team; empty from a
	// pre-org_id instance.
	OrgID string
}

// TeamApproval mints a team-pinned CLI token through a fresh browser
// approval: the instance's /cli-auth page opened with ?team=<id>, where the
// APPROVING live session — never the stored credential — mints the token
// (#2674: a personal access token cannot mint another credential since
// #1668, so the switch re-runs the approval instead of presenting its PAT
// to POST /api/me/tokens).
//
// It reports what the page answered and never decides on it: a result the
// switch refuses is a token already minted, and only its caller can revoke
// it (refuseApprovedToken), so a seam that refused on its own would leave
// that token alive on the server.
type TeamApproval func(ctx context.Context, teamID string) (TeamApprovalResult, error)

// tokenCreateAdvice names the two ways to get a new token.
func tokenCreateAdvice(base string) string {
	return "Create the token in the studio (Account → Access tokens),\n" +
		"or from a fresh login: `iterion remote login " + base + " --email <you>`" +
		" (add `--team <id>` to pin it to a team)."
}

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

// RemoteTokensCreate mints a PAT and prints the plaintext once. A token mints
// no token (patCannotMint), so this refuses before it sends anything when the
// stored credential is one.
func RemoteTokensCreate(ctx context.Context, c *RemoteClient, p *Printer, name, teamID string, expiresInDays int) error {
	advice := tokenCreateAdvice(c.BaseURL())
	if err := c.refuseIfPAT(advice); err != nil {
		return err
	}
	req := map[string]any{"name": name, "expires_in_days": expiresInDays}
	if teamID != "" {
		teamID, err := resolveRemoteTeamID(ctx, c, teamID)
		if err != nil {
			return err
		}
		req["team_id"] = teamID
	}
	var out struct {
		PAT   remotePAT `json:"pat"`
		Token string    `json:"token"`
	}
	raw, err := c.Call(ctx, "POST", "/api/me/tokens", req, &out)
	if err != nil {
		refusal, _ := asPATMintRefusal(err, advice)
		return refusal
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

// teamUUIDShape matches a canonical team UUID — the form the mint pins
// verbatim and every identity path resolves.
var teamUUIDShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resolveRemoteTeamID passes a UUID through untouched (lower-cased — the
// mint pins the value verbatim and the store's lookup is exact) and
// resolves a slug against the account's org tree via /api/auth/me. Team
// NAMES are deliberately not matched: slugs are globally unique, names
// are not, and a silent first-match across orgs pins an identity the
// operator did not choose.
//
// It is the one resolver every command that sends a team_id goes through —
// `tokens create`, `teams switch`, and the login that pins the token it mints
// (LoginWithPassword, under its own JWT) — so a slug the CLI prints in an
// advice is a slug the CLI accepts back.
func resolveRemoteTeamID(ctx context.Context, c *RemoteClient, arg string) (string, error) {
	return resolveRemoteTeamIDAs(ctx, c, "", arg)
}

// resolveRemoteTeamIDAs is resolveRemoteTeamID reading the org tree under an
// explicit bearer: a login resolves the team of the token it is about to mint
// with the JWT it just received, before any credential is stored.
func resolveRemoteTeamIDAs(ctx context.Context, c *RemoteClient, bearer, arg string) (string, error) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if teamUUIDShape.MatchString(arg) {
		return arg, nil
	}
	me, err := c.MeAs(ctx, bearer)
	if err != nil {
		return "", fmt.Errorf("resolving team %q: %w — pass the team UUID (see `iterion remote teams list`)", arg, err)
	}
	hits, found := 0, ""
	incomplete := false
	for _, org := range me.Orgs {
		incomplete = incomplete || org.Unavailable
		for _, t := range org.Teams {
			if arg == strings.ToLower(t.TeamID) || arg == strings.ToLower(t.TeamSlug) {
				hits++
				found = t.TeamID
			}
		}
	}
	if incomplete {
		return "", fmt.Errorf("cannot resolve team %q against an incomplete identity tree: an organization is unavailable — pass the team UUID", arg)
	}
	switch {
	case hits == 1:
		return found, nil
	case hits > 1:
		return "", fmt.Errorf("team %q matches %d teams — pass the team UUID (see `iterion remote teams list`)", arg, hits)
	default:
		return "", fmt.Errorf("no team matches %q by slug or UUID (see `iterion remote teams list`)", arg)
	}
}

// ResolveRemoteRunScopeTeam resolves the --team flag of the run reads (runs
// list, stats, repos, resume --usage-blocked) the way the mint path resolves
// the team it pins: a UUID passes through untouched — no read — a slug
// resolves against the account's org tree. The server reads ?team_id= as an
// id verbatim, so a slug sent as one answers a member and an org admin 403
// "not authorised to scope this route to team_id=<slug>" and a super-admin an
// empty scope; an unresolved slug is refused here, with its name, before
// anything is sent (r7c F8). An empty flag scopes nothing (the caller's
// active team).
func ResolveRemoteRunScopeTeam(ctx context.Context, c *RemoteClient, flag string) (string, error) {
	arg := strings.TrimSpace(flag)
	if arg == "" {
		return "", nil
	}
	return resolveRemoteTeamID(ctx, c, arg)
}

// RemoteTeamsSwitch changes the CLI's default team. A token's identity team
// is pinned at mint time, so switching means minting a NEW token pinned to
// the target team, persisting it, and revoking the previous CLI token
// (identified deterministically by its SHA-256 fingerprint; an env-provided
// or unmatched token is left alone with a notice).
//
// The mint's path depends on what the CLI holds. A session credential mints
// directly (the instance allows it). A personal access token may not mint
// another credential (patCannotMint — #1668), and a PAT is what the CLI
// stores, so the switch re-runs the browser approval with ?team=<id>
// (approve): the page's live session mints the pinned token and hands it to
// the CLI's loopback, the derivation rule holds, and the command stays
// one step (#2674). A nil approve with a PAT stored is the old refusal —
// advice included, nothing sent.
//
// A token the CLI did not mint is a token the CLI must check before it
// stores it: the approving browser's live session may be ANOTHER account's,
// and that account's token would silently become the CLI's identity — every
// later command running as someone else, the replaced token left alive
// because the revocation below would look through the wrong account's
// tokens. So the account is read once BEFORE the approval, the approved
// token is read back from the server, and a mismatch — of the account, of
// the team — refuses without persisting and REVOKES what was minted
// (refuseApprovedToken): a refused switch leaves no live token behind. The
// direct mint needs none of it: there the minter IS the stored credential,
// so neither the account nor the pin can be another than the one asked for.
func RemoteTeamsSwitch(ctx context.Context, c *RemoteClient, p *Printer, teamID, tokenName string, approve TeamApproval) error {
	if strings.TrimSpace(teamID) == "" {
		return fmt.Errorf("team id required (see `iterion remote teams list`)")
	}
	viaBrowser := c.presentsPAT()
	if viaBrowser && approve == nil {
		// Before resolution, which reads /api/auth/me: a credential that
		// cannot mint must send nothing at all. The advice therefore carries
		// the argument as typed — a slug or a UUID — and the login it names
		// resolves it the same way this command would (resolveRemoteTeamID).
		return fmt.Errorf("%s\n%s", patCannotMint, teamSwitchAdvice(c.BaseURL(), strings.TrimSpace(teamID)))
	}
	// Resolution is read-only and comes before the env-mode refusal so the
	// advice it prints carries the UUID, not the raw argument.
	teamID, err := resolveRemoteTeamID(ctx, c, teamID)
	if err != nil {
		return err
	}
	if os.Getenv("ITERION_REMOTE_URL") != "" {
		return fmt.Errorf("teams switch mutates the stored credential and cannot run in ITERION_REMOTE_URL env mode — pin a team at login instead: `iterion remote login %s --email <you> --team %s`", c.BaseURL(), teamID)
	}
	oldFingerprint := secrets.FingerprintSHA256(c.cfg.Token)

	var newToken, orgID string
	if viaBrowser {
		// The approval mints from the approving live session — never from
		// the stored PAT, which this path presents to nothing but the
		// read-only resolution above and the account read here (the
		// revocation below is done with the NEW token).
		before, err := c.Me(ctx)
		if err != nil {
			return fmt.Errorf("read the account the CLI is signed in as, before approving anything: %w — nothing was sent", err)
		}
		res, err := approve(ctx, teamID)
		if err != nil {
			return err
		}
		if res.Token == "" {
			return fmt.Errorf("the browser approval returned no token")
		}
		// What the server says about the token just minted, read WITH it:
		// its account and the team it answers for, in one read. Every
		// refusal from here on revokes it.
		after, err := c.MeAs(ctx, res.Token)
		if err != nil {
			return refuseApprovedToken(ctx, c, res.Token, fmt.Errorf("the approved token could not be read back from %s: %w", c.BaseURL(), err))
		}
		switch {
		case before.User.ID == "" || after.User.ID == "":
			return refuseApprovedToken(ctx, c, res.Token, fmt.Errorf("%s named no account for one of the two credentials, so the approval cannot be shown to be yours", c.BaseURL()))
		case after.User.ID != before.User.ID:
			return refuseApprovedToken(ctx, c, res.Token, fmt.Errorf("the browser approved as %s, not as the account the CLI is signed in as (%s) — sign that account into the studio and retry, or run `iterion remote login %s`",
				describeRemoteAccount(after.User), describeRemoteAccount(before.User), c.BaseURL()))
		case res.TeamID != "" && res.TeamID != teamID:
			return refuseApprovedToken(ctx, c, res.Token, fmt.Errorf("the approval pinned the token to team %q, not the %q it was asked for", res.TeamID, teamID))
		case after.ActiveTeam != teamID:
			return refuseApprovedToken(ctx, c, res.Token, fmt.Errorf("the approved token answers for team %q, not the %q it was asked for — an approval page that predates the team pin mints without one", after.ActiveTeam, teamID))
		}
		newToken, orgID = res.Token, res.OrgID
		// The org the NEW token actually sits in, as the server states it
		// for that token; the page's echo is the fallback for an instance
		// whose /api/auth/me answers no active org.
		if after.ActiveOrg != "" {
			orgID = after.ActiveOrg
		}
	} else {
		// No client-side membership pre-check: the mint endpoint's canViewTeam
		// is the authority (it also grants org-admins and super-admins teams a
		// plain membership listing may not carry). The server states the
		// team’s org in the mint response — it owns that fact.
		var minted struct {
			PAT   remotePAT `json:"pat"`
			Token string    `json:"token"`
			OrgID string    `json:"org_id"`
		}
		req := map[string]any{"name": tokenName, "team_id": teamID, "expires_in_days": 0}
		if _, err := c.Call(ctx, "POST", "/api/me/tokens", req, &minted); err != nil {
			if refusal, ok := asPATMintRefusal(err, teamSwitchAdvice(c.BaseURL(), teamID)); ok {
				return refusal
			}
			return fmt.Errorf("%w (see `iterion remote teams list`)", err)
		}
		if minted.Token == "" {
			return fmt.Errorf("token endpoint returned no token")
		}
		newToken, orgID = minted.Token, minted.OrgID
		p.Line("Switched to team %s (new CLI token %s)", teamID, minted.PAT.ID)
	}

	cfg := c.cfg
	cfg.Token = newToken
	cfg.TeamID = teamID
	// Follow the team into its org so org-scoped commands aim where the
	// new token lives (empty from a pre-org_id server: scope unchanged).
	if orgID != "" {
		cfg.OrgID = orgID
	}
	if err := SaveRemoteConfig(cfg); err != nil {
		return err
	}
	if viaBrowser {
		p.Line("Switched to team %s (token approved in the browser)", teamID)
	}

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

// describeRemoteAccount names an account the way an operator recognises it:
// its email, its id when the view carries none.
func describeRemoteAccount(u server.UserView) string {
	if strings.TrimSpace(u.Email) != "" {
		return u.Email
	}
	return u.ID
}

// refuseApprovedToken is the one way out of a browser approval the switch
// will not store: nothing is persisted, and the token the approval minted is
// revoked — a credential nobody can use must not outlive the refusal that
// rejected it. The error states the reason AND that token's fate, including
// when the revocation itself failed: an operator who is not told cannot
// clean up what they cannot see.
func refuseApprovedToken(ctx context.Context, c *RemoteClient, plaintext string, reason error) error {
	if err := revokeApprovedToken(ctx, c, plaintext); err != nil {
		return fmt.Errorf("%w; nothing was stored, and the token the approval minted could NOT be revoked: %w — revoke it in the studio (Account → Access tokens)", reason, err)
	}
	return fmt.Errorf("%w; nothing was stored and the token the approval minted was revoked", reason)
}

// revokeApprovedToken revokes the token an approval minted, presenting THAT
// token: removing a credential of its own account is a derivation a personal
// access token may make (auth.ChangeCredential), so the refusal needs no
// other credential — which matters, since the token may belong to an account
// the CLI holds nothing for. It matches on the plaintext's own fingerprint,
// so no other token of that account can be hit.
func revokeApprovedToken(ctx context.Context, c *RemoteClient, plaintext string) error {
	cfg := c.cfg
	cfg.Token = plaintext
	client := NewRemoteClientFor(cfg)
	var toks struct {
		Tokens []remotePAT `json:"tokens"`
	}
	if _, err := client.Call(ctx, "GET", "/api/me/tokens", nil, &toks); err != nil {
		return fmt.Errorf("list the tokens of the approving account: %w", err)
	}
	fingerprint := secrets.FingerprintSHA256(plaintext)
	for _, t := range toks.Tokens {
		if t.Fingerprint == "" || t.Fingerprint != fingerprint {
			continue
		}
		if t.RevokedAt != nil {
			return nil
		}
		if _, err := client.Call(ctx, "DELETE", "/api/me/tokens/"+t.ID, nil, nil); err != nil {
			return fmt.Errorf("revoke the minted token %s: %w", t.ID, err)
		}
		return nil
	}
	return fmt.Errorf("no token of the approving account matches the minted one's fingerprint")
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
			if o.Unavailable {
				return fmt.Errorf("organization %q is unavailable; retry after its identity document is repaired", orgID)
			}
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
		ID          string `json:"id"`
		Name        string `json:"name,omitempty"`
		Role        string `json:"role,omitempty"`
		Active      bool   `json:"active"`
		Unavailable bool   `json:"unavailable,omitempty"`
	}
	var orgs []orgRow
	add := func(id, name, role string, unavailable bool) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		orgs = append(orgs, orgRow{ID: id, Name: name, Role: role, Active: id == me.ActiveOrg, Unavailable: unavailable})
	}
	for _, o := range me.Orgs {
		add(o.OrgID, o.OrgName, o.OrgRole, o.Unavailable)
	}
	// The active org is normally in the tree; keep it visible even if not
	// (e.g. an org the account was removed from while its token lives on).
	add(me.ActiveOrg, "", "", false)
	if p.Format == OutputJSON {
		p.JSON(map[string]any{"orgs": orgs})
		return nil
	}
	rows := make([][]string, 0, len(orgs))
	for _, o := range orgs {
		if o.Unavailable {
			o.Name = "Organization unavailable"
		}
		mark := ""
		if o.Active {
			mark = "*"
		}
		rows = append(rows, []string{mark, o.ID, o.Name, o.Role})
	}
	p.Table([]string{"", "ORG ID", "NAME", "ROLE"}, rows)
	return nil
}
