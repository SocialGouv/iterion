package forge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ProjectBotRotationLead is how far before its expiry a minted project bot is
// rotated: a token that lives a year is renewed with two weeks to spare, so a
// rotation the forge refuses leaves time to read the reason and act.
const ProjectBotRotationLead = 14 * 24 * time.Hour

// Project-bot lifetimes are whole days (GitLab's expires_at is a date). The
// floor is the one-week default a rotation without an expiry gets; the cap is
// GitLab's historical maximum.
const (
	projectBotMinLifetime     = 7 * 24 * time.Hour
	projectBotMaxLifetime     = 365 * 24 * time.Hour
	projectBotDefaultLifetime = projectBotMaxLifetime
)

// RotateDueProjectBots rotates every minted project bot expiring within lead
// (ProjectBotRotationLead when zero) — the GitLab twin of RefreshWorker,
// driven by the same ticker. Returns the number rotated; per-bot errors are
// collected but never abort the sweep, and each is also recorded on its
// integration for the operator.
func (o *Orchestrator) RotateDueProjectBots(ctx context.Context, lead time.Duration) (int, error) {
	if lead <= 0 {
		lead = ProjectBotRotationLead
	}
	due, err := o.Integrations.ListProjectBotsExpiringBefore(ctx, o.clock().Add(lead))
	if err != nil {
		return 0, fmt.Errorf("forge: list expiring project bots: %w", err)
	}
	rotated := 0
	var firstErr error
	for _, ri := range due {
		// The sweep crosses tenants; the stores it writes are tenant-scoped.
		did, err := o.rotateProjectBot(store.WithTenant(ctx, ri.TenantID), ri)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if did {
			rotated++
		}
	}
	return rotated, firstErr
}

// rotateProjectBot renews one bot's credential, reporting whether it did.
// The bot rotates ITSELF with the token iterion holds; only when that token
// is already dead does the connection's admin grant rotate it. Write order
// is load-bearing: the forge revoked the old token the moment it issued the
// new one, so the secret is rewritten FIRST — a crash after that leaves a row
// that is still due and a secret that works, which the next sweep
// reconciles; the reverse would leave a row that says "fresh" over a dead
// token.
//
// A bot still inside the first half of its lifetime is left alone even when
// the lead window lists it: on an instance whose maximum lifetime is shorter
// than the lead, every freshly rotated token would otherwise be due again at
// the next tick, and the sweep would rotate it forever.
func (o *Orchestrator) rotateProjectBot(ctx context.Context, ri RepoIntegration) (bool, error) {
	if ri.ProjectBot == nil || ri.ProjectBot.ExpiresAt == nil {
		return false, nil
	}
	now := o.clock()
	lifetime := projectBotLifetime(ri.ProjectBot)
	if ri.ProjectBot.ExpiresAt.Sub(now) > lifetime/2 {
		return false, nil
	}
	conn, err := o.Connections.Get(ctx, ri.ConnectionID)
	if err != nil {
		return false, o.recordProjectBotRotation(ctx, ri, conn, nil, fmt.Errorf("resolve connection %s: %w", ri.ConnectionID, err))
	}
	if ri.ManagedSecretID == "" {
		return false, o.recordProjectBotRotation(ctx, ri, conn, nil, errors.New("the integration holds no project bot secret"))
	}
	gs, err := o.Secrets.Get(ctx, ri.ManagedSecretID)
	if err != nil {
		return false, o.recordProjectBotRotation(ctx, ri, conn, nil, fmt.Errorf("load project bot secret: %w", err))
	}
	expiresAt := now.Add(lifetime)

	cred, err := o.rotateProjectBotSelf(ctx, conn, ri, expiresAt)
	if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrProjectBotUnsupported) {
		cred, err = o.rotateProjectBotAdmin(ctx, conn, ri, expiresAt)
	}
	if err != nil {
		return false, o.recordProjectBotRotation(ctx, ri, conn, nil, err)
	}

	sealed, err := secrets.SealGenericSecret(o.Sealer, gs.ID, []byte(cred.Token))
	if err != nil {
		return false, o.recordProjectBotRotation(ctx, ri, conn, &cred, fmt.Errorf("the forge rotated the token but it could not be sealed: %w", err))
	}
	gs.SealedSecret = sealed
	gs.Last4 = secrets.Last4(cred.Token)
	gs.Fingerprint = secrets.FingerprintSHA256(cred.Token)
	gs.ForgeTokenProof = nil
	if err := o.Secrets.Update(ctx, gs); err != nil {
		return false, o.recordProjectBotRotation(ctx, ri, conn, &cred, fmt.Errorf("the forge rotated the token but the secret rewrite failed, so the stored token is dead until the next sweep: %w", err))
	}

	pb := *ri.ProjectBot
	pb.TokenID = cred.TokenID
	pb.ExpiresAt = nil
	if !cred.ExpiresAt.IsZero() {
		exp := cred.ExpiresAt
		pb.ExpiresAt = &exp
	}
	pb.RotatedAt = &now
	pb.RotationError = ""
	if err := o.Integrations.RecordProjectBot(ctx, ri.ID, ri.ManagedSecretID, ri.ProjectBot.TokenID, &pb, now); err != nil {
		if errors.Is(err, ErrIntegrationNotFound) || errors.Is(err, ErrIntegrationConflict) {
			if rerr := o.revokeUnrecordedRotation(ctx, ri, conn, cred); rerr != nil {
				return false, rerr
			}
			return false, nil
		}
		return false, fmt.Errorf("forge: rotate project bot of %s: the secret carries the new token but the integration update failed (the next sweep rotates again): %w", ri.RepoFullName, err)
	}
	return true, nil
}

// recordProjectBotRotation stamps a failed rotation on the integration so the
// operator reads why a bot is about to expire, and returns the error for the
// sweep's tally. When the forge DID issue a new token (cred != nil) its id is
// recorded while the expiry is left as it was: the row stays due, and the
// next sweep — the stored token now dead — reaches the admin path with the
// id the forge knows, instead of the revoked one.
func (o *Orchestrator) recordProjectBotRotation(ctx context.Context, ri RepoIntegration, conn Connection, cred *ProjectBotCredential, cause error) error {
	pb := *ri.ProjectBot
	if cred != nil {
		pb.TokenID = cred.TokenID
	}
	pb.RotationError = cause.Error()
	if uerr := o.Integrations.RecordProjectBot(ctx, ri.ID, ri.ManagedSecretID, ri.ProjectBot.TokenID, &pb, o.clock()); uerr != nil {
		if cred != nil && (errors.Is(uerr, ErrIntegrationNotFound) || errors.Is(uerr, ErrIntegrationConflict)) {
			if rerr := o.revokeUnrecordedRotation(ctx, ri, conn, *cred); rerr != nil {
				return errors.Join(cause, rerr)
			}
		}
		return fmt.Errorf("forge: rotate project bot of %s: %w (and recording it failed: %v)", ri.RepoFullName, cause, uerr)
	}
	return fmt.Errorf("forge: rotate project bot of %s: %w", ri.RepoFullName, cause)
}

// Both successful and failed secret writes can race a retirement. The issued
// credential belongs to the old identity unless the current record adopted it.
func (o *Orchestrator) revokeUnrecordedRotation(ctx context.Context, ri RepoIntegration, conn Connection, cred ProjectBotCredential) error {
	ctx, cancel := repairContext(ctx)
	defer cancel()
	current, err := o.Integrations.Get(ctx, ri.ID)
	if err != nil && !errors.Is(err, ErrIntegrationNotFound) {
		return err
	}
	if err == nil && current.ProjectBot != nil && current.ProjectBot.TokenID == cred.TokenID && current.ManagedSecretID == ri.ManagedSecretID {
		return nil
	}
	// Use the connection captured before issuance: disconnect can delete its row
	// while this request is still receiving the new credential from the forge.
	if o.AdminForToken == nil {
		return ErrProjectBotUnsupported
	}
	bot, err := o.AdminForToken(conn.Provider, conn.BaseURL(), cred.Token)
	if err != nil {
		return err
	}
	sr, ok := bot.(SelfRotator)
	if !ok {
		return ErrProjectBotUnsupported
	}
	if err := sr.RevokeSelf(ctx); err != nil && !errors.Is(err, ErrUnauthorized) {
		return fmt.Errorf("forge: revoke unrecorded rotated token %s of %s: %w", cred.TokenID, ri.RepoFullName, err)
	}
	return nil
}

func (o *Orchestrator) rotateProjectBotSelf(ctx context.Context, conn Connection, ri RepoIntegration, expiresAt time.Time) (ProjectBotCredential, error) {
	bot, err := o.projectBotClient(ctx, conn, ri)
	if err != nil {
		return ProjectBotCredential{}, err
	}
	sr, ok := bot.(SelfRotator)
	if !ok {
		return ProjectBotCredential{}, ErrProjectBotUnsupported
	}
	return sr.RotateSelf(ctx, ri.RepoFullName, expiresAt)
}

func (o *Orchestrator) rotateProjectBotAdmin(ctx context.Context, conn Connection, ri RepoIntegration, expiresAt time.Time) (ProjectBotCredential, error) {
	admin, err := o.AdminFor(ctx, conn)
	if err != nil {
		return ProjectBotCredential{}, fmt.Errorf("the bot's token is dead and the admin client could not be built: %w", err)
	}
	minter, ok := admin.(ProjectBotMinter)
	if !ok {
		return ProjectBotCredential{}, fmt.Errorf("the bot's token is dead: %w", ErrProjectBotUnsupported)
	}
	cred, err := minter.RotateProjectBot(ctx, ri.RepoFullName, ri.ProjectBot.TokenID, expiresAt)
	if err != nil {
		return ProjectBotCredential{}, fmt.Errorf("the bot's token is dead and the admin grant could not rotate it: %w", err)
	}
	return cred, nil
}

// projectBotLifetime is the lifetime a rotation asks for: the one the forge
// granted last time (expiry minus the mint or the last rotation), in whole
// days, clamped to what GitLab accepts. Unknown — no expiry recorded — reads
// as the maximum.
func projectBotLifetime(pb *ProjectBot) time.Duration {
	if pb == nil || pb.ExpiresAt == nil {
		return projectBotDefaultLifetime
	}
	from := pb.MintedAt
	if pb.RotatedAt != nil && pb.RotatedAt.After(from) {
		from = *pb.RotatedAt
	}
	if from.IsZero() {
		return projectBotDefaultLifetime
	}
	// The expiry is a DATE (midnight UTC, as GitLab's expires_at travels);
	// the mint or rotation happened at some time of that day. Measuring from
	// the day's start keeps the asked lifetime equal to the granted one — a
	// time-of-day start would lose a day per rotation.
	d := pb.ExpiresAt.Sub(from.UTC().Truncate(24 * time.Hour)).Truncate(24 * time.Hour)
	if d < projectBotMinLifetime {
		return projectBotMinLifetime
	}
	if d > projectBotMaxLifetime {
		return projectBotMaxLifetime
	}
	return d
}
