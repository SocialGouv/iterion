package forge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// fakeBotForge is a GitLab-shaped forge: the hook half (fakeAdmin) plus the
// project-access-token API, remembering every token it issued so a client
// built with a token's plaintext (fakeBotClient) can act as that bot.
type fakeBotForge struct {
	*fakeAdmin
	now          func() time.Time
	lifetime     time.Duration // granted at mint; 0 = the forge sets no expiry
	mints        []ProjectBotSpec
	mintErr      error
	rotateErr    error // forced refusal of every rotation
	adminRotates int
	adminRevokes int
	seq          int
	tokens       map[string]*fakeToken // by token id
	byPlain      map[string]string     // plaintext → token id
}

type fakeToken struct {
	id, plain, login, userID string
	expiresAt                time.Time
	revoked                  bool
	avatar                   []byte
	selfRotates, selfRevokes int
}

func newFakeBotForge(now func() time.Time) *fakeBotForge {
	return &fakeBotForge{fakeAdmin: newFakeAdmin(), now: now, lifetime: 365 * 24 * time.Hour,
		tokens: map[string]*fakeToken{}, byPlain: map[string]string{}}
}

func (f *fakeBotForge) issue(expiresAt time.Time) *fakeToken {
	f.seq++
	t := &fakeToken{
		id: fmt.Sprintf("%d", 100+f.seq), plain: fmt.Sprintf("glpat-bot-%d", f.seq),
		login: fmt.Sprintf("project_7_bot_%d", f.seq), userID: "166", expiresAt: expiresAt,
	}
	f.tokens[t.id] = t
	f.byPlain[t.plain] = t.id
	return t
}

func (t *fakeToken) cred() ProjectBotCredential {
	return ProjectBotCredential{TokenID: t.id, Token: t.plain, UserID: t.userID, ExpiresAt: t.expiresAt, Scopes: []string{"api"}, AccessLevel: 40}
}

func (f *fakeBotForge) MintProjectBot(_ context.Context, _ string, spec ProjectBotSpec) (ProjectBotCredential, error) {
	if f.mintErr != nil {
		return ProjectBotCredential{}, f.mintErr
	}
	f.mints = append(f.mints, spec)
	var exp time.Time
	if f.lifetime > 0 {
		exp = f.now().Add(f.lifetime)
	}
	return f.issue(exp).cred(), nil
}

func (f *fakeBotForge) RotateProjectBot(_ context.Context, _, tokenID string, expiresAt time.Time) (ProjectBotCredential, error) {
	f.adminRotates++
	if f.rotateErr != nil {
		return ProjectBotCredential{}, f.rotateErr
	}
	old, ok := f.tokens[tokenID]
	if !ok || old.revoked {
		return ProjectBotCredential{}, ErrUnauthorized // GitLab: a revoked or unknown token answers 401
	}
	old.revoked = true
	return f.issue(expiresAt).cred(), nil
}

func (f *fakeBotForge) RevokeProjectBot(_ context.Context, _, tokenID string) error {
	f.adminRevokes++
	t, ok := f.tokens[tokenID]
	if !ok {
		return &NotFoundError{Provider: ProviderGitLab, Op: "revoke"}
	}
	t.revoked = true
	return nil
}

// clientFor is the orchestrator's AdminForToken: a client AS the token.
func (f *fakeBotForge) clientFor(_ Provider, _, token string) (Admin, error) {
	c := &fakeBotClient{fakeAdmin: f.fakeAdmin, f: f}
	if id, ok := f.byPlain[token]; ok {
		c.tok = f.tokens[id]
	}
	return c, nil
}

func (f *fakeBotForge) live() []*fakeToken {
	var out []*fakeToken
	for _, t := range f.tokens {
		if !t.revoked {
			out = append(out, t)
		}
	}
	return out
}

type fakeBotClient struct {
	*fakeAdmin
	f   *fakeBotForge
	tok *fakeToken
}

func (c *fakeBotClient) dead() bool { return c.tok == nil || c.tok.revoked }

func (c *fakeBotClient) WhoAmI(context.Context) (Identity, error) {
	if c.dead() {
		return Identity{}, ErrUnauthorized
	}
	return Identity{Login: c.tok.login, ID: c.tok.userID, Kind: AccountKindBot, Namespace: c.tok.login}, nil
}

func (c *fakeBotClient) SetAvatar(_ context.Context, png []byte) (string, error) {
	if c.dead() {
		return "", ErrUnauthorized
	}
	c.tok.avatar = png
	return "https://gitlab.example.com/uploads/bot.png", nil
}

func (c *fakeBotClient) RotateSelf(_ context.Context, _ string, expiresAt time.Time) (ProjectBotCredential, error) {
	if c.f.rotateErr != nil {
		return ProjectBotCredential{}, c.f.rotateErr
	}
	if c.dead() {
		return ProjectBotCredential{}, ErrUnauthorized
	}
	c.tok.selfRotates++
	c.tok.revoked = true
	return c.f.issue(expiresAt).cred(), nil
}

func (c *fakeBotClient) RevokeSelf(context.Context) error {
	if c.dead() {
		return ErrUnauthorized
	}
	c.tok.selfRevokes++
	c.tok.revoked = true
	return nil
}

var (
	_ ProjectBotMinter = (*fakeBotForge)(nil)
	_ SelfRotator      = (*fakeBotClient)(nil)
	_ AvatarSetter     = (*fakeBotClient)(nil)
)

const testOAuthGrant = "oauth-grant-of-a-human"

// newProjectBotOrch is newTestOrch on a GitLab OAuth connection that posts as
// per-repository bots, with the token API and the bot-side client wired.
func newProjectBotOrch(t *testing.T) (*Orchestrator, *fakeBotForge, Connection) {
	t.Helper()
	o, _, sealer := newTestOrch(t)
	f := newFakeBotForge(o.clock)
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return f, nil }
	o.AdminForToken = f.clientFor
	o.BotAvatar = []byte("png-bytes")
	o.LogWarn = func(format string, args ...any) { t.Logf("warn: "+format, args...) }

	const connID = "conn-oauth"
	sealed, err := SealOAuthTokens(sealer, connID, testOAuthGrant, "refresh", o.clock().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	conn := Connection{
		ID: connID, TenantID: "t1", Provider: ProviderGitLab, Kind: KindOAuthApp,
		BotIdentity: BotIdentityProjectToken, ForgeBaseURL: "https://gitlab.example.com",
		AccountLogin: "alice", AccountKind: AccountKindUser,
		Status: StatusActive, SealedPayload: sealed, CreatedAt: o.clock(),
	}
	if err := o.Connections.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	return o, f, conn
}

func openSecret(t *testing.T, o *Orchestrator, id string) string {
	t.Helper()
	gs, err := o.Secrets.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("secret %s: %v", id, err)
	}
	raw, err := secrets.OpenGenericSecret(o.Sealer, gs.ID, gs.SealedSecret)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestProvision_ProjectBot_MintsAndPinsItsOwnToken(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	res, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.mints) != 1 {
		t.Fatalf("mints = %d, want 1", len(f.mints))
	}
	spec := f.mints[0]
	if spec.Name != ProjectBotName || spec.AccessLevel != 40 || len(spec.Scopes) != 1 || spec.Scopes[0] != "api" || !spec.ExpiresAt.IsZero() {
		t.Errorf("mint spec = %+v", spec)
	}
	ri, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if ri.ProjectBot == nil {
		t.Fatal("the integration carries no project bot")
	}
	if ri.ProjectBot.TokenID != "101" || ri.ProjectBot.Login != "project_7_bot_1" || ri.ProjectBot.UserID != "166" || ri.ProjectBot.Name != ProjectBotName {
		t.Errorf("project bot = %+v", *ri.ProjectBot)
	}
	if ri.ProjectBot.ExpiresAt == nil || !ri.ProjectBot.ExpiresAt.Equal(o.clock().Add(365*24*time.Hour)) {
		t.Errorf("expires_at = %v, want the forge's expiry", ri.ProjectBot.ExpiresAt)
	}
	if ri.ProjectBot.AvatarAppliedAt == nil || ri.ProjectBot.AvatarError != "" {
		t.Errorf("avatar: applied=%v err=%q", ri.ProjectBot.AvatarAppliedAt, ri.ProjectBot.AvatarError)
	}
	if string(f.tokens["101"].avatar) != "png-bytes" {
		t.Errorf("the iterion-bot avatar was not uploaded with the bot's token")
	}
	if res.BotLogin != "project_7_bot_1" {
		t.Errorf("result bot_login = %q", res.BotLogin)
	}

	// The webhook's pinned forge token IS the minted token — never the grant.
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	secID := cfg.SecretOverrides["forge_token"]
	if secID == "" || secID != ri.ManagedSecretID {
		t.Fatalf("webhook pins %q, integration owns %q", secID, ri.ManagedSecretID)
	}
	if got := openSecret(t, o, secID); got != "glpat-bot-1" {
		t.Errorf("pinned token = %q, want the minted token (the grant is %q)", got, testOAuthGrant)
	}
	if cfg.BotLogin != "project_7_bot_1" {
		t.Errorf("cfg.BotLogin = %q: the actor guard must recognise the bot, not the human", cfg.BotLogin)
	}
	gs, _ := o.Secrets.Get(ctx, secID)
	if gs.Name != "forge_gitlab_connoaut_bot_group_api" || len(gs.AllowedHosts) != 1 || gs.AllowedHosts[0] != "gitlab.example.com" {
		t.Errorf("secret name/egress = %q %v", gs.Name, gs.AllowedHosts)
	}

	// No team-global binding (a per-repo token cannot be the team's one
	// answer), and the connection keeps no managed secret of its own.
	if b, _ := o.Bindings.ListByTenantBot(ctx, "t1", "review-pr"); len(b) != 0 {
		t.Errorf("bindings = %+v, want none", b)
	}
	got, _ := o.Connections.Get(ctx, conn.ID)
	if got.ManagedSecretID != "" {
		t.Errorf("connection.ManagedSecretID = %q, want none", got.ManagedSecretID)
	}
}

func TestProvision_ProjectBot_ReprovisionReusesTheBot(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	first, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"revi-converse"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.mints) != 1 {
		t.Fatalf("mints = %d: a rebuild must reuse the bot", len(f.mints))
	}
	if second.ManagedSecretID != first.ManagedSecretID || second.BotLogin != first.BotLogin {
		t.Errorf("rebuild changed the credential: %+v vs %+v", first, second)
	}
	cfg, _ := o.Webhooks.Get(ctx, second.WebhookID)
	if cfg.BotLogin != first.BotLogin || cfg.SecretOverrides["forge_token"] != first.ManagedSecretID {
		t.Errorf("rebuilt config: BotLogin=%q pin=%q", cfg.BotLogin, cfg.SecretOverrides["forge_token"])
	}
	// The idempotent no-op path names the bot too and writes no binding.
	third, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr", "revi-converse"}, ActorID: "u1"})
	if err != nil || third.Created || third.BotLogin != first.BotLogin {
		t.Errorf("idempotent provision: %+v err=%v", third, err)
	}
	if b, _ := o.Bindings.ListByTenantBot(ctx, "t1", "review-pr"); len(b) != 0 {
		t.Errorf("bindings after the no-op = %+v, want none", b)
	}
}

func TestProvision_ProjectBot_HookRefusalRevokesTheMint(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.createErr = errors.New("gitlab: create hook: HTTP 422: Invalid url given")
	_, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err == nil {
		t.Fatal("the provision must fail on the refused hook")
	}
	if len(f.mints) != 1 {
		t.Fatalf("mints = %d", len(f.mints))
	}
	if live := f.live(); len(live) != 0 {
		t.Errorf("the minted token survived the failed provision: %+v", live[0])
	}
	if f.tokens["101"].selfRevokes != 1 {
		t.Errorf("self revokes = %d: the bot revokes itself with the token iterion holds", f.tokens["101"].selfRevokes)
	}
	if list, _ := o.Secrets.ListByTeam(ctx, "t1", ""); len(list) != 0 {
		t.Errorf("secrets left behind: %+v", list)
	}
	if _, gerr := o.Integrations.GetByConnRepo(ctx, "t1", conn.ID, "group/api"); !errors.Is(gerr, ErrIntegrationNotFound) {
		t.Errorf("integration recorded despite the failure: %v", gerr)
	}
}

func TestProvision_ProjectBot_MintRefusalIsTypedAndWritesNothing(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.mintErr = fmt.Errorf("%w: gitlab answered HTTP 400: User does not have permission to create project access token", ErrProjectBotNotPermitted)
	_, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if !errors.Is(err, ErrProjectBotNotPermitted) {
		t.Fatalf("err = %v, want ErrProjectBotNotPermitted", err)
	}
	if f.creates != 0 {
		t.Errorf("a hook was created before the mint refusal")
	}
	if list, _ := o.Secrets.ListByTeam(ctx, "t1", ""); len(list) != 0 {
		t.Errorf("secrets written: %+v", list)
	}
	if cfgs, _ := o.Webhooks.ListByTenant(ctx, "t1"); len(cfgs) != 0 {
		t.Errorf("webhook configs written: %d", len(cfgs))
	}
}

func TestProvision_ProjectBot_UnsupportedClientIsRefused(t *testing.T) {
	o, _, conn := newProjectBotOrch(t)
	plain := newFakeAdmin() // no token API
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return plain, nil }
	_, err := o.Provision(context.Background(), ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if !errors.Is(err, ErrProjectBotUnsupported) {
		t.Fatalf("err = %v, want ErrProjectBotUnsupported", err)
	}
}

func TestDeprovision_ProjectBot_RevokesAndDeletesItsSecret(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	res, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Deprovision(ctx, "t1", res.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if live := f.live(); len(live) != 0 {
		t.Errorf("token still live after deprovision: %+v", live[0])
	}
	if f.tokens["101"].selfRevokes != 1 || f.adminRevokes != 0 {
		t.Errorf("self=%d admin=%d: the bot revokes itself first", f.tokens["101"].selfRevokes, f.adminRevokes)
	}
	if _, err := o.Secrets.Get(ctx, res.ManagedSecretID); !errors.Is(err, secrets.ErrGenericSecretNotFound) {
		t.Errorf("the bot's secret survived: %v", err)
	}
	if f.deletes != 1 {
		t.Errorf("hook deletes = %d", f.deletes)
	}
}

func TestDeprovision_ProjectBot_DeadTokenFallsBackToTheAdminGrant(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	res, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	// The forge expired the token behind iterion's back: self-revoke answers
	// 401, which only says the token iterion HOLDS is dead — the admin grant
	// still addresses the token id the row records, and its answer decides.
	f.tokens["101"].revoked = true
	if err := o.Deprovision(ctx, "t1", res.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if f.adminRevokes != 1 {
		t.Errorf("admin revokes = %d: a 401 to the bot must fall through to the grant", f.adminRevokes)
	}
	// A bot the orchestrator cannot act AS (no client for it) goes through
	// the admin grant.
	o2, f2, conn2 := newProjectBotOrch(t)
	res2, err := o2.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn2.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	o2.AdminForToken = nil
	if err := o2.Deprovision(ctx, "t1", res2.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if f2.adminRevokes != 1 || len(f2.live()) != 0 {
		t.Errorf("admin revokes = %d live=%d", f2.adminRevokes, len(f2.live()))
	}
}

func TestProvision_LegacyConnectionIsUntouchedByTheBotPath(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	conn := seedConn(t, o, sealer) // PAT connection: posts as its own account
	f := newFakeBotForge(o.clock)
	f.fakeAdmin = fa
	o.AdminFor = func(context.Context, Connection) (Admin, error) { return f, nil }
	o.AdminForToken = f.clientFor
	ctx := context.Background()
	res, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.mints) != 0 {
		t.Errorf("a connection that posts as itself minted a bot")
	}
	cfg, _ := o.Webhooks.Get(ctx, res.WebhookID)
	if cfg.BotLogin != "" {
		t.Errorf("cfg.BotLogin = %q, want empty", cfg.BotLogin)
	}
	if got := openSecret(t, o, cfg.SecretOverrides["forge_token"]); got != testPATToken {
		t.Errorf("pinned token = %q, want the connection's own", got)
	}
	if b, _ := o.Bindings.ListByTenantBot(ctx, "t1", "review-pr"); len(b) != 1 {
		t.Errorf("bindings = %d, want the team binding as before", len(b))
	}
	ri, _ := o.Integrations.Get(ctx, res.IntegrationID)
	if ri.ProjectBot != nil || res.BotLogin != "" {
		t.Errorf("project bot recorded on a legacy integration: %+v", ri.ProjectBot)
	}
}

func TestManagedSecretFor(t *testing.T) {
	o, _, conn := newProjectBotOrch(t)
	ctx := context.Background()
	if _, err := o.ManagedSecretFor(ctx, &conn, "group/api", "u1"); !errors.Is(err, ErrRepoNotEnabled) {
		t.Fatalf("not enabled: err = %v, want ErrRepoNotEnabled", err)
	}
	res, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := o.ManagedSecretFor(ctx, &conn, "group/api", "u1")
	if err != nil || got != res.ManagedSecretID {
		t.Errorf("enabled: %q err=%v, want the integration's secret %q", got, err, res.ManagedSecretID)
	}
	if _, err := o.ManagedSecretFor(ctx, &conn, "group/other", "u1"); !errors.Is(err, ErrRepoNotEnabled) {
		t.Errorf("another repo: err = %v", err)
	}
	// A connection that posts as itself keeps the connection-level secret.
	legacyOrch, _, sealer := newTestOrch(t)
	legacy := seedConn(t, legacyOrch, sealer)
	secID, err := legacyOrch.ManagedSecretFor(ctx, &legacy, "group/api", "u1")
	if err != nil || secID == "" || legacy.ManagedSecretID != secID {
		t.Errorf("legacy: %q err=%v conn=%q", secID, err, legacy.ManagedSecretID)
	}
}

func TestSecretSlug(t *testing.T) {
	cases := map[string]string{
		"group/api": "group_api",
		"SocialGouv/produits-dnum/citoyens/vao/vao": "socialgouv_produits_dnum_citoyens_vao_vao",
		"  weird//name.git  ":                       "weird_name_git",
		"a-very-long-namespace/with-many-segments/and-a-repository-name-that-goes-on": "a_very_long_namespace_with_many_segments_and_a_r",
	}
	for in, want := range cases {
		if got := secretSlug(in); got != want {
			t.Errorf("secretSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// After a rotation whose secret rewrite failed, the row names the NEW token
// while the secret still holds the dead old one. Disabling the repo must
// revoke the token the row names through the admin grant — a 401 to the bot
// only says the token iterion holds is dead.
func TestDeprovision_ProjectBot_RowTokenRevokedWhenTheHeldTokenIsDead(t *testing.T) {
	o, f, conn := newProjectBotOrch(t)
	ctx := context.Background()
	f.lifetime = tenDays
	ri := age(t, o, provisionProjectBot(t, o, conn, "group/api"), tenDays, threeDays)
	fs := &failingSecretUpdate{GenericSecretStore: o.Secrets, fail: true}
	o.Secrets = fs
	if n, err := o.RotateDueProjectBots(ctx, 0); err == nil || n != 0 {
		t.Fatalf("n=%d err=%v, want the rewrite failure", n, err)
	}
	got, _ := o.Integrations.Get(ctx, ri.ID)
	if got.ProjectBot.TokenID == ri.ProjectBot.TokenID {
		t.Fatal("the row must name the rotated token")
	}
	if err := o.Deprovision(ctx, "t1", ri.ID); err != nil {
		t.Fatal(err)
	}
	if live := f.live(); len(live) != 0 {
		t.Errorf("the token the row named is still live: %+v", *live[0])
	}
	if f.adminRevokes != 1 {
		t.Errorf("admin revokes = %d, want the fall-through to the grant", f.adminRevokes)
	}
}
