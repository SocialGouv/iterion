package forge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// --- fake admin client ---

type fakeAdmin struct {
	provider   Provider
	hooks      map[string]HookHandle // keyed by repo (one iterion hook per repo)
	nextID     int
	creates    int
	updates    int
	deletes    int
	gets       int
	lastSecret string
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{provider: ProviderGitLab, hooks: map[string]HookHandle{}}
}

func (f *fakeAdmin) Provider() Provider { return f.provider }
func (f *fakeAdmin) WhoAmI(context.Context) (Identity, error) {
	return Identity{Login: "bot", Kind: "user"}, nil
}
func (f *fakeAdmin) ListRepos(context.Context, RepoQuery) ([]RepoSummary, error) {
	return nil, nil
}

func (f *fakeAdmin) GetHook(_ context.Context, repo, url string) (*HookHandle, error) {
	f.gets++
	h, ok := f.hooks[repo]
	if !ok || h.URL != url {
		return nil, nil
	}
	cp := h
	return &cp, nil
}

func (f *fakeAdmin) CreateHook(_ context.Context, repo string, spec HookSpec) (HookHandle, error) {
	f.creates++
	f.nextID++
	f.lastSecret = spec.Secret
	h := HookHandle{ID: fmt.Sprintf("hook-%d", f.nextID), URL: spec.URL, Events: spec.Events, Active: spec.Active}
	f.hooks[repo] = h
	return h, nil
}

func (f *fakeAdmin) UpdateHook(_ context.Context, repo, hookID string, spec HookSpec) (HookHandle, error) {
	f.updates++
	f.lastSecret = spec.Secret
	h := HookHandle{ID: hookID, URL: spec.URL, Events: spec.Events, Active: spec.Active}
	f.hooks[repo] = h
	return h, nil
}

func (f *fakeAdmin) DeleteHook(_ context.Context, repo, hookID string) error {
	f.deletes++
	delete(f.hooks, repo)
	return nil
}

// --- fixtures ---

func testBotLookup(botID string) (*bundle.ForgeRequirements, error) {
	switch botID {
	case "review-pr":
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventPullRequest, bundle.ForgeEventPullRequestComment},
			TokenScopes: map[string]string{"pull_requests": "write", "repository": "read"},
			Secret:      "forge_token",
			Webhook:     &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"pr_review_mode": "summary", "post_to_board": "false"}},
		}, nil
	case "revi-converse":
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventPullRequestComment},
			TokenScopes: map[string]string{"pull_requests": "write", "repository": "read"},
			Secret:      "forge_token",
			Webhook:     &bundle.ForgeWebhookHints{MinReplierRole: "developer"},
		}, nil
	case "dep-guard":
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventPullRequest, bundle.ForgeEventPullRequestComment},
			TokenScopes: map[string]string{"pull_requests": "write", "repository": "write"},
			Secret:      "forge_token",
			Webhook: &bundle.ForgeWebhookHints{
				AuthorAllowlist: []string{"dependabot[bot]", "renovate[bot]"},
			},
		}, nil
	case "no-forge-bot":
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown bot %q", botID)
	}
}

const testPATToken = "glpat-secret-token-1234567890"

func newTestOrch(t *testing.T) (*Orchestrator, *fakeAdmin, secrets.Sealer) {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fa := newFakeAdmin()
	var idc int
	o := &Orchestrator{
		Connections:  NewMemoryConnectionStore(),
		Integrations: NewMemoryRepoIntegrationStore(),
		Webhooks:     webhooks.NewMemoryConfigStore(),
		Secrets:      secrets.NewMemoryGenericSecretStore(),
		Sealer:       sealer,
		Bots:         testBotLookup,
		AdminFor:     func(context.Context, Connection) (Admin, error) { return fa, nil },
		PublicURL:    "https://iterion.example.com",
		Now:          func() time.Time { return time.Unix(1700000000, 0).UTC() },
		NewID:        func() string { idc++; return fmt.Sprintf("id-%d", idc) },
	}
	return o, fa, sealer
}

func seedConn(t *testing.T, o *Orchestrator, sealer secrets.Sealer) Connection {
	t.Helper()
	const connID = "conn-1"
	sealed, err := sealConnectionSecret(sealer, connID, connectionSecret{PATToken: testPATToken})
	if err != nil {
		t.Fatal(err)
	}
	c := Connection{
		ID:            connID,
		TenantID:      "t1",
		Provider:      ProviderGitLab,
		Kind:          KindPAT,
		ForgeBaseURL:  "https://gitlab.example.com",
		Status:        StatusActive,
		SealedPayload: sealed,
		CreatedAt:     time.Unix(1699000000, 0).UTC(),
	}
	if err := o.Connections.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func sameSet(a, b []string) bool {
	ac := append([]string{}, a...)
	bc := append([]string{}, b...)
	sort.Strings(ac)
	sort.Strings(bc)
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// --- tests ---

func TestProvision_SingleBot(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !res.Created {
		t.Error("expected Created=true on fresh provision")
	}

	// managed secret created from the connection's PAT, team-scoped.
	conn, _ := o.Connections.Get(ctx, "conn-1")
	if conn.ManagedSecretID == "" {
		t.Fatal("connection ManagedSecretID not stamped")
	}
	gs, err := o.Secrets.Get(ctx, conn.ManagedSecretID)
	if err != nil {
		t.Fatalf("get managed secret: %v", err)
	}
	if gs.ScopeTeamID != "t1" || gs.ScopeUserID != "" {
		t.Errorf("managed secret scope: team=%q user=%q", gs.ScopeTeamID, gs.ScopeUserID)
	}
	pt, err := secrets.OpenGenericSecret(sealer, gs.ID, gs.SealedSecret)
	if err != nil || string(pt) != testPATToken {
		t.Errorf("managed secret plaintext = %q (err %v), want the PAT", string(pt), err)
	}

	// webhook config.
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if cfg.Provider != webhooks.ProviderGitLab {
		t.Errorf("provider = %q", cfg.Provider)
	}
	if cfg.SignMode != webhooks.SignModeToken {
		t.Errorf("gitlab sign mode should be token, got %q", cfg.SignMode)
	}
	if !sameSet(cfg.BotIDs, []string{"review-pr"}) {
		t.Errorf("bot ids = %v", cfg.BotIDs)
	}
	if !sameSet(cfg.EventAllowlist, []string{"merge_request", "note"}) {
		t.Errorf("event allowlist = %v", cfg.EventAllowlist)
	}
	if !sameSet(cfg.ProjectAllowlist, []string{"group/api"}) {
		t.Errorf("project allowlist = %v", cfg.ProjectAllowlist)
	}
	if cfg.ForgeBaseURL != "https://gitlab.example.com" {
		t.Errorf("forge base url = %q", cfg.ForgeBaseURL)
	}
	if cfg.SecretOverrides["forge_token"] != conn.ManagedSecretID {
		t.Errorf("secret override forge_token = %q, want %q", cfg.SecretOverrides["forge_token"], conn.ManagedSecretID)
	}
	if cfg.ProvisionedBy != "forge:conn-1" {
		t.Errorf("provisioned_by = %q", cfg.ProvisionedBy)
	}
	if cfg.LaunchVars["pr_review_mode"] != "summary" {
		t.Errorf("launch var pr_review_mode = %q", cfg.LaunchVars["pr_review_mode"])
	}
	if cfg.TokenHash == "" || cfg.TokenLast4 == "" {
		t.Error("webhook token not minted")
	}

	// forge hook created with the minted secret.
	if fa.creates != 1 {
		t.Errorf("CreateHook calls = %d, want 1", fa.creates)
	}
	h := fa.hooks["group/api"]
	wantURL := "https://iterion.example.com/api/webhooks/gitlab/" + res.WebhookID
	if h.URL != wantURL {
		t.Errorf("hook url = %q, want %q", h.URL, wantURL)
	}
	if !sameSet(h.Events, []string{"merge_request", "note"}) {
		t.Errorf("hook events = %v", h.Events)
	}
	if fa.lastSecret == "" {
		t.Error("hook secret (iwh_) was empty")
	}

	// integration row.
	ri, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatalf("get integration: %v", err)
	}
	if ri.WebhookID != res.WebhookID || ri.HookID != h.ID || ri.ManagedSecretID != conn.ManagedSecretID {
		t.Errorf("integration links wrong: %+v", ri)
	}
}

// A dependency-guard bot's author_allowlist propagates to the webhook
// Config, scoping the auto-created hook to the dependency bots.
func TestProvision_AuthorAllowlist(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config: %v", err)
	}
	if !sameSet(cfg.AuthorAllowlist, []string{"dependabot[bot]", "renovate[bot]"}) {
		t.Errorf("author allowlist = %v, want the dep bots", cfg.AuthorAllowlist)
	}

	// Co-enabling a bot that reviews all authors (review-pr, empty allowlist)
	// must re-open the shared webhook so its human PRs aren't silently dropped.
	res2, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api",
		BotIDs: []string{"dep-guard", "review-pr"}, ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("provision (add review-pr): %v", err)
	}
	cfg2, err := o.Webhooks.Get(ctx, res2.WebhookID)
	if err != nil {
		t.Fatalf("get webhook config 2: %v", err)
	}
	if len(cfg2.AuthorAllowlist) != 0 {
		t.Errorf("author allowlist = %v, want empty (open) when a review-all bot is co-enabled", cfg2.AuthorAllowlist)
	}
}

func TestProvision_Idempotent(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	req := ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"}

	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatal(err)
	}
	res2, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created {
		t.Error("second identical provision should be a no-op (Created=false)")
	}
	if fa.creates != 1 || fa.updates != 0 {
		t.Errorf("idempotent re-run touched the forge: creates=%d updates=%d", fa.creates, fa.updates)
	}
}

func TestProvision_AddSecondBot(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()

	if _, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"}); err != nil {
		t.Fatal(err)
	}
	res, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"revi-converse"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Error("adding a bot to an existing integration is not a fresh create")
	}
	if fa.creates != 1 {
		t.Errorf("CreateHook should not be called again, got %d", fa.creates)
	}
	if fa.updates < 1 {
		t.Error("hook should have been updated to widen events")
	}
	cfg, _ := o.Webhooks.Get(ctx, res.WebhookID)
	if !sameSet(cfg.BotIDs, []string{"review-pr", "revi-converse"}) {
		t.Errorf("bot ids after add = %v", cfg.BotIDs)
	}
	if cfg.MinReplierRole != "developer" {
		t.Errorf("min replier role = %q, want developer", cfg.MinReplierRole)
	}
	ri, _ := o.Integrations.Get(ctx, res.IntegrationID)
	if !sameSet(ri.BotIDs, []string{"review-pr", "revi-converse"}) {
		t.Errorf("integration bots = %v", ri.BotIDs)
	}
}

func TestDeprovision(t *testing.T) {
	o, fa, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	res, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := o.Connections.Get(ctx, "conn-1")
	managedID := conn.ManagedSecretID

	if err := o.Deprovision(ctx, "t1", res.IntegrationID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	if fa.deletes != 1 || len(fa.hooks) != 0 {
		t.Errorf("forge hook not deleted: deletes=%d hooks=%d", fa.deletes, len(fa.hooks))
	}
	if _, err := o.Webhooks.Get(ctx, res.WebhookID); !errors.Is(err, webhooks.ErrNotFound) {
		t.Errorf("webhook config should be gone, got %v", err)
	}
	if _, err := o.Integrations.Get(ctx, res.IntegrationID); !errors.Is(err, ErrIntegrationNotFound) {
		t.Errorf("integration should be gone, got %v", err)
	}
	// managed secret is connection-level — survives a single deprovision.
	if _, err := o.Secrets.Get(ctx, managedID); err != nil {
		t.Errorf("managed secret should survive deprovision, got %v", err)
	}
}

func TestDeprovisionConnection_Cascades(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	ctx := context.Background()
	res, err := o.Provision(ctx, ProvisionRequest{TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := o.Connections.Get(ctx, "conn-1")
	managedID := conn.ManagedSecretID

	if err := o.DeprovisionConnection(ctx, "t1", "conn-1"); err != nil {
		t.Fatalf("deprovision connection: %v", err)
	}
	if _, err := o.Integrations.Get(ctx, res.IntegrationID); !errors.Is(err, ErrIntegrationNotFound) {
		t.Error("integration should be gone after connection teardown")
	}
	if _, err := o.Secrets.Get(ctx, managedID); !errors.Is(err, secrets.ErrGenericSecretNotFound) {
		t.Error("managed secret should be deleted with the connection")
	}
	if _, err := o.Connections.Get(ctx, "conn-1"); !errors.Is(err, ErrConnectionNotFound) {
		t.Error("connection should be gone")
	}
}

func TestProvision_CrossTenantRejected(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "other", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"review-pr"},
	})
	if !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("cross-tenant provision should be ErrConnectionNotFound, got %v", err)
	}
}

func TestProvision_BotWithoutForgeBlock(t *testing.T) {
	o, _, sealer := newTestOrch(t)
	seedConn(t, o, sealer)
	_, err := o.Provision(context.Background(), ProvisionRequest{
		TenantID: "t1", ConnectionID: "conn-1", RepoFullName: "group/api", BotIDs: []string{"no-forge-bot"},
	})
	if err == nil {
		t.Fatal("expected error for a bot with no forge: block")
	}
}
