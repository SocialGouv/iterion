package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// Project access tokens are GitLab's per-project bot identity: creating one
// creates a bot user (`project_<id>_bot_<hex>`) that joins the project at the
// requested role, and the token authenticates as that user. iterion mints one
// per enabled repository on a connection whose BotIdentity is
// BotIdentityProjectToken, so reviews are posted by the repository's own bot
// and never by the person whose OAuth grant bootstrapped the connection.
//
// The API, as GitLab serves it (lib/api/resource_access_tokens.rb):
//   - POST   /projects/:id/access_tokens                     — mint; refusals are 400
//     carrying the cause in `message` ("User does not have permission to
//     create project access token" covers both a role below Maintainer and
//     tokens switched off on the group/instance). `expires_at` (YYYY-MM-DD)
//     always travels: with "Require personal access token expiry" on (recent
//     GitLab's default), ResourceAccessTokens::CreateService refuses a
//     request without the key ("expires_at is missing") before any default
//     applies, and a date past the instance or group ceiling (365 days unless
//     a policy lowers it) is a 400 "must be before <date>".
//   - POST   /projects/:id/access_tokens/:token_id/rotate    — the admin grant
//     rotates; `self` lets the token rotate itself (scope api). The old token
//     is revoked the moment the new one exists; without `expires_at` the new
//     one lives ONE WEEK, so the caller always says the expiry it wants.
//   - DELETE /projects/:id/access_tokens/:token_id           — revoke (204; 404 gone).
//   - DELETE /personal_access_tokens/self                    — the token revokes itself.

// gitlabDate is the YYYY-MM-DD shape `expires_at` travels in both directions.
const gitlabDate = "2006-01-02"

// accessToken is the wire shape of a project access token. `token` is only
// present on the create and rotate answers.
type accessToken struct {
	ID          int64    `json:"id"`
	UserID      int64    `json:"user_id"`
	Token       string   `json:"token"`
	ExpiresAt   string   `json:"expires_at"`
	Scopes      []string `json:"scopes"`
	AccessLevel int      `json:"access_level"`
}

// credential turns a create/rotate answer into the provider-neutral
// credential, refusing an answer that carries no token: a 2xx with nothing to
// seal would otherwise store an empty secret and fail three hours later.
func (t accessToken) credential(op string) (forge.ProjectBotCredential, error) {
	if t.Token == "" {
		return forge.ProjectBotCredential{}, fmt.Errorf("gitlab: %s: the answer carries no token", op)
	}
	cred := forge.ProjectBotCredential{
		TokenID:     strconv.FormatInt(t.ID, 10),
		Token:       t.Token,
		UserID:      strconv.FormatInt(t.UserID, 10),
		Scopes:      t.Scopes,
		AccessLevel: t.AccessLevel,
	}
	if t.ExpiresAt != "" {
		d, err := time.Parse(gitlabDate, t.ExpiresAt)
		if err != nil {
			return forge.ProjectBotCredential{}, fmt.Errorf("gitlab: %s: expires_at %q is not a date: %w", op, t.ExpiresAt, err)
		}
		cred.ExpiresAt = d.UTC()
	}
	return cred, nil
}

// accessTokenErr types a non-2xx answer to a token operation. A 400 on the
// mint is GitLab's refusal of the caller's rights (ErrProjectBotNotPermitted,
// the message says which); every other status keeps the sentinels the rest of
// the package classifies on, with GitLab's own message as the detail.
func accessTokenErr(op string, code int, body []byte) error {
	if code == http.StatusBadRequest && op == opMintProjectBot {
		return fmt.Errorf("%w: gitlab answered HTTP 400 to %s: %s", forge.ErrProjectBotNotPermitted, op, forge.RefusalMessage(body))
	}
	err := statusErr(op, code)
	var se *forge.StatusError
	if errors.As(err, &se) && se.Detail == "" {
		se.Detail = forge.RefusalMessage(body)
	}
	return err
}

const (
	opMintProjectBot   = "create project access token"
	opRotateProjectBot = "rotate project access token"
	opRevokeProjectBot = "revoke project access token"
)

// lifetimeLadder lists the lifetimes, in days from the UTC date, asked for
// when GitLab refuses a date, longest first: GitLab's own ceiling
// (MAX_PERSONAL_ACCESS_TOKEN_LIFETIME_IN_DAYS = 365, a date up to today + 365
// is accepted), 364 for an instance whose own date is a day behind UTC, then
// shorter ones for an instance or group policy that lowers the ceiling.
// GitLab judges each step; no refusal message is parsed. Each date GitLab
// refuses on a mint leaves a bot user and its membership that GitLab deletes
// asynchronously — the price of asking rather than assuming a policy.
var lifetimeLadder = []int{365, 364, 180, 90, 30, 7}

// ladderDown asks for expiresAt first, then for every lifetimeLadder step
// strictly shorter than the lifetime asked, and keeps the first token GitLab
// grants. A 400 moves down; any other status, a transport error and a 2xx
// without a token return at once — a second request after an answer that
// may have minted would mint twice. An exhausted ladder returns the last
// typed refusal, with GitLab's own message.
func ladderDown(expiresAt time.Time, try func(time.Time) (forge.ProjectBotCredential, int, error)) (forge.ProjectBotCredential, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	asked := int(expiresAt.UTC().Truncate(24*time.Hour).Sub(today).Hours() / 24)
	cred, code, err := try(expiresAt)
	if err == nil || code != http.StatusBadRequest {
		return cred, err
	}
	for _, days := range lifetimeLadder {
		if days >= asked {
			continue
		}
		if cred, code, err = try(today.AddDate(0, 0, days)); err == nil || code != http.StatusBadRequest {
			return cred, err
		}
	}
	return forge.ProjectBotCredential{}, err
}

// MintProjectBot creates the project access token (and its bot user) with the
// connection's admin grant. The request always carries `expires_at`: a GitLab
// that requires the key refuses a request without it before any default
// applies. A spec that names an expiry asks for that date, a zero one for
// GitLab's 365-day ceiling; either way a date GitLab refuses is followed by
// the shorter ladder steps (ladderDown). A refusal that survives the ladder —
// the grant below Maintainer, tokens switched off, a policy under the
// shortest step — is returned typed with GitLab's last message.
func (c *AdminClient) MintProjectBot(ctx context.Context, repo string, spec forge.ProjectBotSpec) (forge.ProjectBotCredential, error) {
	expiresAt := spec.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, lifetimeLadder[0])
	}
	return ladderDown(expiresAt, func(d time.Time) (forge.ProjectBotCredential, int, error) {
		return c.mintProjectBotOnce(ctx, repo, spec, d)
	})
}

// mintProjectBotOnce issues one create request asking for expiresAt and
// returns the HTTP status alongside the typed error so the ladder can tell a
// refusal of the date (400) from one of the grant (401, 404, …).
func (c *AdminClient) mintProjectBotOnce(ctx context.Context, repo string, spec forge.ProjectBotSpec, expiresAt time.Time) (forge.ProjectBotCredential, int, error) {
	body := map[string]any{
		"name":         spec.Name,
		"scopes":       spec.Scopes,
		"access_level": spec.AccessLevel,
		"expires_at":   expiresAt.UTC().Format(gitlabDate),
	}
	var out accessToken
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/access_tokens", body, &out)
	if err != nil {
		return forge.ProjectBotCredential{}, 0, err
	}
	if code/100 != 2 {
		return forge.ProjectBotCredential{}, code, accessTokenErr(opMintProjectBot, code, errBody)
	}
	cred, err := out.credential(opMintProjectBot)
	return cred, code, err
}

// RotateProjectBot rotates the token through the admin grant.
func (c *AdminClient) RotateProjectBot(ctx context.Context, repo, tokenID string, expiresAt time.Time) (forge.ProjectBotCredential, error) {
	return c.rotateAccessToken(ctx, repo, url.PathEscape(tokenID), expiresAt)
}

// RotateSelf rotates the token this client authenticates with — the minted
// bot acting on itself, no admin grant involved.
func (c *AdminClient) RotateSelf(ctx context.Context, repo string, expiresAt time.Time) (forge.ProjectBotCredential, error) {
	return c.rotateAccessToken(ctx, repo, "self", expiresAt)
}

// rotateAccessToken asks for expiresAt — the lifetime granted at mint — and,
// when GitLab refuses the date because a policy shrank since, the shorter
// ladder steps: a bot whose rotation kept asking the old lifetime would die at
// expiry. A zero expiry sends no date (GitLab then issues a one-week token).
func (c *AdminClient) rotateAccessToken(ctx context.Context, repo, tokenRef string, expiresAt time.Time) (forge.ProjectBotCredential, error) {
	if expiresAt.IsZero() {
		cred, _, err := c.rotateAccessTokenOnce(ctx, repo, tokenRef, expiresAt)
		return cred, err
	}
	return ladderDown(expiresAt, func(d time.Time) (forge.ProjectBotCredential, int, error) {
		return c.rotateAccessTokenOnce(ctx, repo, tokenRef, d)
	})
}

func (c *AdminClient) rotateAccessTokenOnce(ctx context.Context, repo, tokenRef string, expiresAt time.Time) (forge.ProjectBotCredential, int, error) {
	body := map[string]any{}
	if !expiresAt.IsZero() {
		body["expires_at"] = expiresAt.UTC().Format(gitlabDate)
	}
	var out accessToken
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/access_tokens/"+tokenRef+"/rotate", body, &out)
	if err != nil {
		return forge.ProjectBotCredential{}, 0, err
	}
	if code/100 != 2 {
		return forge.ProjectBotCredential{}, code, accessTokenErr(opRotateProjectBot, code, errBody)
	}
	cred, err := out.credential(opRotateProjectBot)
	return cred, code, err
}

// RevokeProjectBot revokes the token through the admin grant. A 404 answers
// the ErrNotFound class: the token is already gone, which is what the caller
// wanted.
func (c *AdminClient) RevokeProjectBot(ctx context.Context, repo, tokenID string) error {
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodDelete, "/projects/"+projectID(repo)+"/access_tokens/"+url.PathEscape(tokenID), nil, nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return accessTokenErr(opRevokeProjectBot, code, errBody)
	}
	return nil
}

// RevokeSelf revokes the token this client authenticates with. A token that
// is already dead answers 401, which the caller reads as done.
func (c *AdminClient) RevokeSelf(ctx context.Context) error {
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodDelete, "/personal_access_tokens/self", nil, nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return accessTokenErr(opRevokeProjectBot, code, errBody)
	}
	return nil
}

var (
	_ forge.ProjectBotMinter = (*AdminClient)(nil)
	_ forge.SelfRotator      = (*AdminClient)(nil)
)
