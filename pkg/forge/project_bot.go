package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The per-repository bot iterion mints on a connection whose BotIdentity is
// BotIdentityProjectToken. The name is what the forge shows on every review
// the bot posts; the role is the floor for hooks and commit statuses; the
// scope is GitLab's one write scope.
const (
	ProjectBotName        = "iterion-bot"
	ProjectBotAccessLevel = 40 // GitLab Maintainer
)

// ProjectBotScopes is the scope set of a minted project bot token.
var ProjectBotScopes = []string{"api"}

// projectBotLogin is the login a webhook config carries as BotLogin: the
// minted bot's, or nothing when the connection posts as its own account.
func projectBotLogin(pb *ProjectBot) string {
	if pb == nil {
		return ""
	}
	return pb.Login
}

// projectBotSecretName names the integration's own generic secret after the
// connection's managed-secret name, so the team's secret list reads which
// connection and which repository a minted token serves.
func projectBotSecretName(conn *Connection, repo string) string {
	return managedSecretName(conn) + "_bot_" + secretSlug(repo)
}

// secretSlug folds a repository path into the [a-z0-9_] alphabet secret
// names are read in, capped so a deep namespace stays a readable label.
func secretSlug(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case lastUnderscore:
		default:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 48 {
		out = strings.TrimRight(out[:48], "_")
	}
	return out
}

// mintProjectBot creates the repository's bot credential with the
// connection's admin grant, reads the bot's own identity with the minted
// token, seals the token as the integration's own generic secret and
// (best-effort) gives the bot the iterion-bot face. Everything after the mint
// is compensated: a credential this call cannot record is revoked before the
// error is returned, so a failed enable leaves no Maintainer token that only
// the forge knows about.
func (o *Orchestrator) mintProjectBot(ctx context.Context, conn Connection, repo, actor string) (*ProjectBot, string, error) {
	admin, err := o.AdminFor(ctx, conn)
	if err != nil {
		return nil, "", fmt.Errorf("forge: build admin client: %w", err)
	}
	minter, ok := admin.(ProjectBotMinter)
	if !ok || o.AdminForToken == nil {
		return nil, "", fmt.Errorf("%w: connection %s (%s) asks for a per-repository bot", ErrProjectBotUnsupported, conn.ID, conn.Provider)
	}
	cred, err := minter.MintProjectBot(ctx, repo, ProjectBotSpec{
		Name: ProjectBotName, AccessLevel: ProjectBotAccessLevel, Scopes: ProjectBotScopes,
	})
	if err != nil {
		return nil, "", fmt.Errorf("forge: mint project bot on %s: %w", repo, err)
	}
	undo := func(cause error) error {
		if rerr := minter.RevokeProjectBot(ctx, repo, cred.TokenID); rerr != nil && !errors.Is(rerr, ErrNotFound) {
			return fmt.Errorf("%w (and revoking the minted token %s failed, revoke it on the forge: %v)", cause, cred.TokenID, rerr)
		}
		return cause
	}
	bot, err := o.AdminForToken(conn.Provider, conn.BaseURL(), cred.Token)
	if err != nil {
		return nil, "", undo(fmt.Errorf("forge: build the project bot client: %w", err))
	}
	// The minted token answers WhoAmI as the bot user: its login is what the
	// actor guard must recognise, and a token that cannot say who it is was
	// not worth storing.
	ident, err := bot.WhoAmI(ctx)
	if err != nil {
		return nil, "", undo(fmt.Errorf("forge: read the minted bot's identity: %w", err))
	}
	now := o.clock()
	secID := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(o.Sealer, secID, []byte(cred.Token))
	if err != nil {
		return nil, "", undo(fmt.Errorf("forge: seal project bot token: %w", err))
	}
	gs := secrets.GenericSecret{
		ID:           secID,
		TenantID:     conn.TenantID,
		ScopeTeamID:  conn.TenantID,
		Name:         projectBotSecretName(&conn, repo),
		Last4:        secrets.Last4(cred.Token),
		Fingerprint:  secrets.FingerprintSHA256(cred.Token),
		SealedSecret: sealed,
		AllowedHosts: forgeTokenEgressHosts(&conn), // egress lock, see forgeTokenEgressHosts
		CreatedBy:    actor,
		CreatedAt:    now,
	}
	if err := o.Secrets.Create(ctx, gs); err != nil {
		return nil, "", undo(fmt.Errorf("forge: create project bot secret: %w", err))
	}
	pb := &ProjectBot{
		TokenID:  cred.TokenID,
		Login:    ident.Login,
		UserID:   ident.ID,
		Name:     ProjectBotName,
		MintedAt: now,
	}
	if pb.UserID == "" {
		pb.UserID = cred.UserID
	}
	if !cred.ExpiresAt.IsZero() {
		exp := cred.ExpiresAt
		pb.ExpiresAt = &exp
	}
	// A fresh bot user has no avatar of its own, so there is nothing to
	// preserve: the rebrand is unconditional, and a refusal is recorded on the
	// bot rather than failing the enable over a picture.
	if len(o.BotAvatar) > 0 {
		if setter, ok := bot.(AvatarSetter); ok {
			if _, aerr := setter.SetAvatar(ctx, o.BotAvatar); aerr != nil {
				pb.AvatarError = aerr.Error()
			} else {
				pb.AvatarAppliedAt = &now
			}
		}
	}
	return pb, secID, nil
}

// undoProjectBot compensates a mint that nothing committed: the token is
// revoked and the secret deleted. Best-effort by nature — the provision
// already failed for another reason, which is the error the caller gets —
// but never silent: a bot that could not be revoked is named for the operator
// to remove on the forge.
func (o *Orchestrator) undoProjectBot(ctx context.Context, conn Connection, repo string, pb *ProjectBot, secretID string) {
	ri := RepoIntegration{TenantID: conn.TenantID, ConnectionID: conn.ID, RepoFullName: repo, ManagedSecretID: secretID, ProjectBot: pb}
	if err := o.revokeProjectBotFor(ctx, conn, ri); err != nil && o.LogWarn != nil {
		o.LogWarn("forge: connection %s: the project bot minted on %s (token %s, @%s) could not be revoked after the provision failed — revoke it on the forge (Settings → Access tokens): %v",
			conn.ID, repo, pb.TokenID, pb.Login, err)
	}
	if err := o.Secrets.Delete(ctx, secretID); err != nil && !errors.Is(err, secrets.ErrGenericSecretNotFound) && o.LogWarn != nil {
		o.LogWarn("forge: connection %s: the project bot secret %s minted for %s could not be deleted after the provision failed: %v", conn.ID, secretID, repo, err)
	}
}

// revokeProjectBot revokes an integration's minted credential on the forge.
func (o *Orchestrator) revokeProjectBot(ctx context.Context, ri RepoIntegration) error {
	conn, err := o.Connections.Get(ctx, ri.ConnectionID)
	if err != nil {
		return err
	}
	return o.revokeProjectBotFor(ctx, conn, ri)
}

// revokeProjectBotFor revokes the credential first AS the bot (the token
// iterion holds is the one credential that is always there) and then, unless
// the bot itself confirmed the revocation, through the connection's admin
// grant, addressing the token id the ROW records. The two are not the same
// token when a rotation's secret rewrite failed (the row names the new id,
// the secret holds the dead old one): a 401 to the bot says only that the
// token iterion HOLDS is dead, so the admin revoke still runs, and its 404 is
// the one answer that means gone. A grant that is dead too cannot reach the
// forge: that is reported, not hidden, and the disable goes on — the operator
// is told which token to revoke by hand.
func (o *Orchestrator) revokeProjectBotFor(ctx context.Context, conn Connection, ri RepoIntegration) error {
	if ri.ProjectBot == nil {
		return nil
	}
	var selfErr error
	if bot, err := o.projectBotClient(ctx, conn, ri); err != nil {
		selfErr = err
	} else if sr, ok := bot.(SelfRotator); !ok {
		selfErr = ErrProjectBotUnsupported
	} else if err := sr.RevokeSelf(ctx); err == nil {
		return nil
	} else {
		selfErr = err
	}
	admin, err := o.AdminFor(ctx, conn)
	if err != nil {
		return fmt.Errorf("self-revoke failed (%v) and the admin client could not be built: %w", selfErr, err)
	}
	minter, ok := admin.(ProjectBotMinter)
	if !ok {
		return fmt.Errorf("self-revoke failed (%v): %w", selfErr, ErrProjectBotUnsupported)
	}
	switch err := minter.RevokeProjectBot(ctx, ri.RepoFullName, ri.ProjectBot.TokenID); {
	case err == nil, errors.Is(err, ErrNotFound):
		return nil
	case errors.Is(err, ErrUnauthorized) && errors.Is(selfErr, ErrUnauthorized):
		// Both credentials are dead for the forge: nothing iterion holds can
		// reach the token the row names, if it is still alive at all.
		if o.LogWarn != nil {
			o.LogWarn("forge: %s: neither the bot's token nor the connection's grant can revoke project access token %s (@%s) — if it is still active, revoke it on the forge (Settings → Access tokens)",
				ri.RepoFullName, ri.ProjectBot.TokenID, ri.ProjectBot.Login)
		}
		return nil
	default:
		return fmt.Errorf("self-revoke failed (%v), admin revoke of token %s failed: %w", selfErr, ri.ProjectBot.TokenID, err)
	}
}

// projectBotClient opens the integration's secret and builds a client that
// authenticates AS the minted bot.
func (o *Orchestrator) projectBotClient(ctx context.Context, conn Connection, ri RepoIntegration) (Admin, error) {
	if o.AdminForToken == nil {
		return nil, ErrProjectBotUnsupported
	}
	if ri.ManagedSecretID == "" {
		return nil, fmt.Errorf("forge: integration %s holds no project bot secret", ri.ID)
	}
	gs, err := o.Secrets.Get(ctx, ri.ManagedSecretID)
	if err != nil {
		return nil, fmt.Errorf("forge: load project bot secret: %w", err)
	}
	raw, err := secrets.OpenGenericSecret(o.Sealer, gs.ID, gs.SealedSecret)
	if err != nil {
		return nil, fmt.Errorf("forge: open project bot secret: %w", err)
	}
	return o.AdminForToken(conn.Provider, conn.BaseURL(), string(raw))
}

// ManagedSecretFor is the runtime forge token a launch on (conn, repo) pins:
// the connection's managed secret — created on demand, re-minted at the
// point of use for a GitHub App — or, on a connection that posts as
// per-repository bots, the repository's own minted credential. There the
// repository must already be enabled: the bot is minted by the enable and
// nothing else can stand in for it, so a launch on a repo without one is
// refused with ErrRepoNotEnabled rather than handed the person's grant.
func (o *Orchestrator) ManagedSecretFor(ctx context.Context, conn *Connection, repoFullName, actor string) (string, error) {
	if !conn.MintsProjectBots() {
		return o.EnsureManagedSecret(ctx, conn, actor)
	}
	ri, err := o.Integrations.GetByConnRepo(ctx, conn.TenantID, conn.ID, repoFullName)
	switch {
	case errors.Is(err, ErrIntegrationNotFound):
		return "", fmt.Errorf("%w: %s on connection %s posts as a per-repository bot that is minted when the repository is enabled — enable it first", ErrRepoNotEnabled, repoFullName, conn.ID)
	case err != nil:
		return "", err
	case ri.ProjectBot == nil || ri.ManagedSecretID == "":
		return "", fmt.Errorf("%w: %s on connection %s carries no minted bot — re-run the enable, which mints it", ErrRepoNotEnabled, repoFullName, conn.ID)
	}
	return ri.ManagedSecretID, nil
}
