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
//     tokens switched off on the group/instance).
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

// MintProjectBot creates the project access token (and its bot user) with the
// connection's admin grant. An omitted expiry leaves GitLab's default — the
// instance's maximum lifetime, which is what the caller wants for a bot it
// rotates itself.
func (c *AdminClient) MintProjectBot(ctx context.Context, repo string, spec forge.ProjectBotSpec) (forge.ProjectBotCredential, error) {
	body := map[string]any{
		"name":         spec.Name,
		"scopes":       spec.Scopes,
		"access_level": spec.AccessLevel,
	}
	if !spec.ExpiresAt.IsZero() {
		body["expires_at"] = spec.ExpiresAt.UTC().Format(gitlabDate)
	}
	var out accessToken
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/access_tokens", body, &out)
	if err != nil {
		return forge.ProjectBotCredential{}, err
	}
	if code/100 != 2 {
		return forge.ProjectBotCredential{}, accessTokenErr(opMintProjectBot, code, errBody)
	}
	return out.credential(opMintProjectBot)
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

func (c *AdminClient) rotateAccessToken(ctx context.Context, repo, tokenRef string, expiresAt time.Time) (forge.ProjectBotCredential, error) {
	body := map[string]any{}
	if !expiresAt.IsZero() {
		body["expires_at"] = expiresAt.UTC().Format(gitlabDate)
	}
	var out accessToken
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/access_tokens/"+tokenRef+"/rotate", body, &out)
	if err != nil {
		return forge.ProjectBotCredential{}, err
	}
	if code/100 != 2 {
		return forge.ProjectBotCredential{}, accessTokenErr(opRotateProjectBot, code, errBody)
	}
	return out.credential(opRotateProjectBot)
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
