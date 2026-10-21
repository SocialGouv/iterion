package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// provisionProjectBot enables one repo on a project-bot connection and
// returns its integration.
func provisionProjectBot(t *testing.T, o *Orchestrator, conn Connection, repo string) RepoIntegration {
	t.Helper()
	res, err := o.Provision(context.Background(), ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: repo, BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	ri, err := o.Integrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	return ri
}

// age moves a minted bot's row to "lifetime - remaining has elapsed": the
// clock is fixed in tests, so elapsed time is written onto the row instead.
func age(t *testing.T, o *Orchestrator, ri RepoIntegration, lifetime, remaining time.Duration) RepoIntegration {
	t.Helper()
	now := o.clock()
	minted, exp := now.Add(remaining-lifetime), now.Add(remaining)
	ri.ProjectBot.MintedAt, ri.ProjectBot.ExpiresAt = minted, &exp
	if err := o.Integrations.Update(context.Background(), ri); err != nil {
		t.Fatal(err)
	}
	return ri
}

const (
	day         = 24 * time.Hour
	tenDays     = 10 * day
	threeDays   = 3 * day
	sixtyDays   = 60 * day
	defaultLead = 14 * day
)

func TestRotateDueProjectBots_DueSelection(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	soon := age(t, o, provisionProjectBot(t, o, conn, "group/soon"), tenDays, threeDays)
	f.lifetime = sixtyDays
	later := provisionProjectBot(t, o, conn, "group/later")
	// Listed by the lead window, but still inside the first half of its
	// life: left alone, or a short-lived token would be rotated every tick.
	f.lifetime = tenDays
	fresh := provisionProjectBot(t, o, conn, "group/fresh")
	f.lifetime = 0
	never := provisionProjectBot(t, o, conn, "group/never")

	n, err := o.RotateDueProjectBots(ctx, defaultLead)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rotated = %d, want only the bot past half its life and inside the lead", n)
	}
	for _, c := range []struct {
		ri      RepoIntegration
		rotated bool
	}{{soon, true}, {later, false}, {fresh, false}, {never, false}} {
		got, _ := o.Integrations.Get(ctx, c.ri.ID)
		if (got.ProjectBot.RotatedAt != nil) != c.rotated {
			t.Errorf("%s: rotated=%v, want %v", c.ri.RepoFullName, got.ProjectBot.RotatedAt != nil, c.rotated)
		}
	}
}

func TestRotateDueProjectBots_SelfRotationRewritesSecretThenRow(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	oldTokenID := ri.ProjectBot.TokenID

	if _, err := o.RotateDueProjectBots(ctx, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := o.Integrations.Get(ctx, ri.ID)
	pb := got.ProjectBot
	if pb.TokenID == oldTokenID || pb.RotatedAt == nil || pb.RotationError != "" {
		t.Fatalf("row after rotation: %+v", *pb)
	}
	if f.tokens[oldTokenID].selfRotates != 1 || f.adminRotates != 0 {
		t.Errorf("self=%d admin=%d: the bot rotates itself", f.tokens[oldTokenID].selfRotates, f.adminRotates)
	}
	// The secret holds the NEW token; the old one is dead on the forge.
	if plain := openSecret(t, o, got.ManagedSecretID); plain != f.tokens[pb.TokenID].plain {
		t.Errorf("secret = %q, want the rotated token", plain)
	}
	if !f.tokens[oldTokenID].revoked {
		t.Errorf("the old token is still live")
	}
	// The lifetime asked for is the one the forge granted at mint (10 days),
	// not the one-week default a bare rotation would get.
	if want := o.clock().Add(tenDays); pb.ExpiresAt == nil || !pb.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", pb.ExpiresAt, want)
	}
	if pb.Login != ri.ProjectBot.Login || pb.MintedAt != ri.ProjectBot.MintedAt {
		t.Errorf("rotation changed the bot's identity: %+v", *pb)
	}
	// Not due any more.
	if n, _ := o.RotateDueProjectBots(ctx, 0); n != 0 {
		t.Errorf("rotated again: %d", n)
	}
}

func TestRotateDueProjectBots_DeadTokenRotatesThroughTheAdminGrant(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	// iterion was down past the expiry: the stored token is dead for the forge
	// but the admin grant still knows the token id.
	stale := f.tokens[ri.ProjectBot.TokenID]
	stale.revoked = false
	f.byPlain[stale.plain] = "" // the forge no longer recognises the plaintext → 401 on self
	if _, err := o.RotateDueProjectBots(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if f.adminRotates != 1 {
		t.Errorf("admin rotates = %d", f.adminRotates)
	}
	got, _ := o.Integrations.Get(ctx, ri.ID)
	if got.ProjectBot.TokenID == stale.id || got.ProjectBot.RotationError != "" {
		t.Errorf("row = %+v", *got.ProjectBot)
	}
	if plain := openSecret(t, o, got.ManagedSecretID); plain != f.tokens[got.ProjectBot.TokenID].plain {
		t.Errorf("secret = %q", plain)
	}
}

func TestRotateDueProjectBots_RefusalIsRecordedAndReturned(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	before := openSecret(t, o, ri.ManagedSecretID)
	f.rotateErr = errors.New("gitlab: rotate project access token: HTTP 400: Expiration date must be before the maximum allowed lifetime")

	n, err := o.RotateDueProjectBots(ctx, 0)
	if err == nil || n != 0 {
		t.Fatalf("n=%d err=%v, want the refusal", n, err)
	}
	got, _ := o.Integrations.Get(ctx, ri.ID)
	if got.ProjectBot.RotationError == "" || got.ProjectBot.RotatedAt != nil || got.ProjectBot.TokenID != ri.ProjectBot.TokenID {
		t.Errorf("row = %+v", *got.ProjectBot)
	}
	if after := openSecret(t, o, ri.ManagedSecretID); after != before {
		t.Errorf("the secret changed on a refused rotation")
	}
	// Still due: the next sweep retries, and a lifted refusal clears the error.
	f.rotateErr = nil
	if n, err := o.RotateDueProjectBots(ctx, 0); err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	got, _ = o.Integrations.Get(ctx, ri.ID)
	if got.ProjectBot.RotationError != "" || got.ProjectBot.RotatedAt == nil {
		t.Errorf("row after the retry = %+v", *got.ProjectBot)
	}
}

// failingSecretUpdate refuses the next Update, standing in for a store blip
// between the forge's rotation and the secret rewrite.
type failingSecretUpdate struct {
	secrets.GenericSecretStore
	fail bool
}

func (f *failingSecretUpdate) Update(ctx context.Context, s secrets.GenericSecret) error {
	if f.fail {
		f.fail = false
		return errors.New("store unavailable")
	}
	return f.GenericSecretStore.Update(ctx, s)
}

func TestRotateDueProjectBots_SecretRewriteFailureStaysDueWithTheNewTokenID(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	oldID := ri.ProjectBot.TokenID
	fs := &failingSecretUpdate{GenericSecretStore: o.Secrets, fail: true}
	o.Secrets = fs

	if n, err := o.RotateDueProjectBots(ctx, 0); err == nil || n != 0 {
		t.Fatalf("n=%d err=%v, want the rewrite failure", n, err)
	}
	got, _ := o.Integrations.Get(ctx, ri.ID)
	pb := got.ProjectBot
	if pb.TokenID == oldID || pb.RotationError == "" || pb.RotatedAt != nil {
		t.Fatalf("row = %+v: the new token id must be recorded, the expiry kept, the error named", *pb)
	}
	if !pb.ExpiresAt.Equal(*ri.ProjectBot.ExpiresAt) {
		t.Errorf("expires_at moved to %v: the row must stay due", pb.ExpiresAt)
	}
	// Next sweep: the stored (old) token is dead → the admin grant rotates the
	// recorded id, and everything converges.
	if n, err := o.RotateDueProjectBots(ctx, 0); err != nil || n != 1 {
		t.Fatalf("recovery: n=%d err=%v", n, err)
	}
	got, _ = o.Integrations.Get(ctx, ri.ID)
	if got.ProjectBot.RotationError != "" || got.ProjectBot.RotatedAt == nil {
		t.Errorf("row after recovery = %+v", *got.ProjectBot)
	}
	if plain := openSecret(t, o, got.ManagedSecretID); plain != f.tokens[got.ProjectBot.TokenID].plain {
		t.Errorf("secret = %q", plain)
	}
	if f.adminRotates != 1 {
		t.Errorf("admin rotates = %d", f.adminRotates)
	}
}

func TestProjectBotLifetime(t *testing.T) {
	base := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := base.Add(d); return &x }
	cases := []struct {
		name string
		pb   *ProjectBot
		want time.Duration
	}{
		{"nil", nil, projectBotMaxLifetime},
		{"no expiry", &ProjectBot{MintedAt: base}, projectBotMaxLifetime},
		{"granted at mint", &ProjectBot{MintedAt: base, ExpiresAt: at(90 * day)}, 90 * day},
		{"after a rotation the last rotation dates it", &ProjectBot{MintedAt: base, RotatedAt: at(80 * day), ExpiresAt: at(170 * day)}, 90 * day},
		{"below the floor", &ProjectBot{MintedAt: base, ExpiresAt: at(2 * day)}, projectBotMinLifetime},
		{"above the cap", &ProjectBot{MintedAt: base, ExpiresAt: at(400 * day)}, projectBotMaxLifetime},
		{"partial day truncates", &ProjectBot{MintedAt: base, ExpiresAt: at(30*day + 5*time.Hour)}, 30 * day},
		// GitLab's expiry is a date; the mint happened at 22:13 that day. The
		// granted lifetime is 30 whole days, and a rotation must ask for 30 —
		// not 29, then 28, down to the floor.
		{"minted late in the day keeps the granted lifetime", &ProjectBot{MintedAt: base.Add(22*time.Hour + 13*time.Minute), ExpiresAt: at(30 * day)}, 30 * day},
		{"rotated late in the day keeps the granted lifetime", &ProjectBot{MintedAt: base, RotatedAt: at(15*day + 23*time.Hour), ExpiresAt: at(45 * day)}, 30 * day},
	}
	for _, c := range cases {
		if got := projectBotLifetime(c.pb); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A deprovision that wins the race against a rotation: the row the sweep
// read is gone when it writes it back. The token the sweep obtained is
// recorded nowhere, so the sweep revokes it rather than leaving a live
// Maintainer token only the forge knows about.
func TestRotateDueProjectBots_RowGoneMidRotationRevokesTheNewToken(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	// The store answers the listing, then loses the row before the write back.
	o.Integrations = &vanishingIntegrations{RepoIntegrationStore: o.Integrations, id: ri.ID}
	if n, err := o.RotateDueProjectBots(ctx, 0); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want a quiet no-op", n, err)
	}
	if live := f.live(); len(live) != 0 {
		t.Errorf("token still live after the row vanished: %+v", *live[0])
	}
}

// vanishingIntegrations serves one listing, then forgets the row on Update.
type vanishingIntegrations struct {
	RepoIntegrationStore
	id string
}

func (v *vanishingIntegrations) Update(ctx context.Context, ri RepoIntegration) error {
	if ri.ID == v.id {
		return ErrIntegrationNotFound
	}
	return v.RepoIntegrationStore.Update(ctx, ri)
}
