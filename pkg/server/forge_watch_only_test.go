package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// seedConnForPublish creates a github connection on github.com with a chosen
// role, so the publish resolver can be asked which one it picks.
func seedConnForPublish(t *testing.T, s *Server, id string, purpose forge.Purpose) {
	t.Helper()
	conn := forge.Connection{
		ID: id, TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, AccountLogin: "iterion-forge-x[bot]",
		InstallationAccount: "SocialGouv", InstallationID: 42,
		ForgeBaseURL: "https://github.com", Purpose: purpose,
	}
	if err := s.forgeConnections.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
}

// forgeConnectionForPR falls back to "the first team connection on this forge
// host". A watch-only connection sits on that same host and would be picked —
// then every review comment and commit status posted through it 403s, because
// its App holds no pull_requests/statuses grant by design. The failure would
// look like a broken reviewer, not like a mis-picked connection.
func TestForgeConnectionForPR_SkipsWatchOnlyConnection(t *testing.T) {
	s := newForgeTestServer(t)
	seedConnForPublish(t, s, "watch-only", forge.PurposeSecurityRead)

	if got, ok := s.forgeConnectionForPR(context.Background(), "t1", "", "github.com", "SocialGouv/x"); ok {
		t.Fatalf("resolver picked the watch-only connection %q — it cannot post anything", got.ID)
	}

	// With a runtime connection present it must resolve to that one.
	seedConnForPublish(t, s, "runtime", forge.PurposeRuntime)
	got, ok := s.forgeConnectionForPR(context.Background(), "t1", "", "github.com", "SocialGouv/x")
	if !ok || got.ID != "runtime" {
		t.Fatalf("resolver = (%q, %v), want the runtime connection", got.ID, ok)
	}
}

// Explicitly naming a watch-only connection must not bypass the guard either:
// the preferred-id path runs through the same predicate.
func TestForgeConnectionForPR_PreferredWatchOnlyIsRefused(t *testing.T) {
	s := newForgeTestServer(t)
	seedConnForPublish(t, s, "watch-only", forge.PurposeSecurityRead)
	if got, ok := s.forgeConnectionForPR(context.Background(), "t1", "watch-only", "github.com", "SocialGouv/x"); ok {
		t.Fatalf("pinned watch-only connection %q was accepted", got.ID)
	}
}

// #662: forgeConnectionForPR dereferences s.forgeConnections; every
// sibling caller (publish, pending, reconcile) already guarded first, the
// approve lane didn't, and the probe panicked (forge_publish.go:794). The
// class-wide fix moves the nil guard INSIDE the helper.
func TestForgeConnectionForPR_NilStoreReturnsFalseDoesNotPanic(t *testing.T) {
	s := &Server{}
	// No forgeConnections wired.
	got, ok := s.forgeConnectionForPR(context.Background(), "t1", "", "github.com", "SocialGouv/x")
	if ok {
		t.Fatalf("nil store must return (empty, false), got %v", got)
	}
}

// #662: fallback picks the LATEST connection on the host, not the first.
// ListByTenant sorts created_at ascending on both stores, so a repo
// re-provisioned onto a newer connection would inherit the stale one — the
// sibling repoIntegrationForRepo already takes the latest for the same
// reason. Shared helper serves publish + pending + reconcile too.
func TestForgeConnectionForPR_FallbackPicksLatestConnection(t *testing.T) {
	s := newForgeTestServer(t)
	// The older connection was replaced by a newer one on the same host.
	older := forge.Connection{
		ID: "conn-older", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: "https://github.com",
		Purpose: forge.PurposeRuntime, CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	newer := forge.Connection{
		ID: "conn-newer", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: "https://github.com",
		Purpose: forge.PurposeRuntime, CreatedAt: time.Now().Add(-1 * time.Hour),
	}
	if err := s.forgeConnections.Create(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeConnections.Create(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	got, ok := s.forgeConnectionForPR(context.Background(), "t1", "", "github.com", "SocialGouv/x")
	if !ok {
		t.Fatal("resolver returned no match")
	}
	if got.ID != "conn-newer" {
		t.Fatalf("fallback picked %q, want the LATEST connection conn-newer — publish/pending/reconcile all read through this", got.ID)
	}
}

// The health DTO is assembled at TWO endpoints (the health view and the
// refresh route) and rendered by the same card. Guarding only the site in
// front of you leaves the other one telling the operator to "fix" the very
// property that makes a watch-only App safe to install org-wide — which is
// how this guard was shipped the first time.
func TestMissingDeliveryFor_SilentOnWatchOnlyLoudOnRuntime(t *testing.T) {
	watchOnlyGrant := map[string]string{"metadata": "read", "vulnerability_alerts": "read"}

	watch := forge.Connection{Purpose: forge.PurposeSecurityRead}
	if got := missingDeliveryFor(watch, watchOnlyGrant); len(got) != 0 {
		t.Fatalf("watch-only reported missing delivery grants %v — they are the point, not a defect", got)
	}
	// The same grant on a RUNTIME connection is a genuine gap and must show.
	runtime := forge.Connection{Purpose: forge.PurposeRuntime}
	if got := missingDeliveryFor(runtime, watchOnlyGrant); len(got) == 0 {
		t.Fatal("a runtime connection missing its delivery grants reported nothing")
	}
}

// The opt-in endpoint serves ANY github_app connection — the ordinary-App path
// is the documented one. On a RUNTIME connection AccessTokenExpiresAt is the
// clock of the runtime token sealed in the payload, and a security token always
// expires an hour out: writing it there pushes the connection out of the
// refresh sweep for ~55 minutes, letting a runtime token that was near expiry
// die unrenewed while every bot on it 401s.
func TestPatchSecurityRead_DoesNotMoveARuntimeConnectionsRefreshClock(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "runtime-conn", "SocialGouv", "", false)

	// A runtime token about to expire — the case that breaks.
	conn, err := s.forgeConnections.Get(context.Background(), "runtime-conn")
	if err != nil {
		t.Fatal(err)
	}
	nearly := time.Now().UTC().Add(2 * time.Minute)
	conn.AccessTokenExpiresAt = &nearly
	if err := s.forgeConnections.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, time.Time, error) {
		return "ghs_minted", time.Now().UTC().Add(time.Hour), nil
	}
	if w := patchSecurityRead(t, s, "runtime-conn", true); w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}

	got, err := s.forgeConnections.Get(context.Background(), "runtime-conn")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessTokenExpiresAt == nil || !got.AccessTokenExpiresAt.Equal(nearly) {
		t.Fatalf("runtime refresh clock moved to %v (was %v) — the runtime token will die unrenewed",
			got.AccessTokenExpiresAt, nearly)
	}
}

// On a watch-only connection the same field IS the refresh clock and nothing
// else writes it, so the mint's expiry must land.
func TestPatchSecurityRead_DatesAWatchOnlyConnectionFromTheMint(t *testing.T) {
	s := newForgeTestServer(t)
	seedAppConn(t, s, "watch-conn", "SocialGouv", "", false)
	conn, err := s.forgeConnections.Get(context.Background(), "watch-conn")
	if err != nil {
		t.Fatal(err)
	}
	conn.Purpose = forge.PurposeSecurityRead
	if err := s.forgeConnections.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	s.forgeSecurityMint = func(context.Context, forge.Connection) (string, time.Time, error) {
		return "ghs_minted", until, nil
	}
	if w := patchSecurityRead(t, s, "watch-conn", true); w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := s.forgeConnections.Get(context.Background(), "watch-conn")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessTokenExpiresAt == nil || !got.AccessTokenExpiresAt.Equal(until) {
		t.Fatalf("watch-only clock = %v, want the mint's expiry %v", got.AccessTokenExpiresAt, until)
	}
}

// The repo's newest integration can sit on a watch-only connection (the
// security-read App is provisioned on the same repos it watches). Selecting
// that row and then rejecting its connection sends the resolver to the
// host-wide fallback — the latest runtime connection on the host, which
// does not cover the repo — instead of the older integration that does;
// and the launch-context policy would be read off the watch-only row. The
// watch-only filter belongs in the integration lookup itself.
func TestRepoIntegrationFor_SkipsWatchOnlyRows(t *testing.T) {
	s := newForgeTestServer(t)
	ctx := context.Background()
	now := time.Now()
	for _, c := range []forge.Connection{
		{ID: "conn-a", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp, Status: forge.StatusActive, ForgeBaseURL: "https://github.com", Purpose: forge.PurposeRuntime, CreatedAt: now.Add(-72 * time.Hour)},
		{ID: "conn-b", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp, Status: forge.StatusActive, ForgeBaseURL: "https://github.com", Purpose: forge.PurposeRuntime, CreatedAt: now.Add(-48 * time.Hour)},
		{ID: "conn-watch", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp, Status: forge.StatusActive, ForgeBaseURL: "https://github.com", Purpose: forge.PurposeSecurityRead, CreatedAt: now.Add(-1 * time.Hour)},
	} {
		if err := s.forgeConnections.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, ri := range []forge.RepoIntegration{
		{ID: "ri-a", TenantID: "t1", ConnectionID: "conn-a", RepoFullName: "SocialGouv/x", CreatedAt: now.Add(-72 * time.Hour), LaunchVars: map[string]string{"gate_context": "iterion/review"}},
		{ID: "ri-watch", TenantID: "t1", ConnectionID: "conn-watch", RepoFullName: "SocialGouv/x", CreatedAt: now.Add(-1 * time.Hour)},
	} {
		if err := s.forgeIntegrations.Create(ctx, ri); err != nil {
			t.Fatal(err)
		}
	}
	ri, ok := s.repoIntegrationFor(ctx, "t1", "github.com", "SocialGouv/x")
	if !ok || ri.ID != "ri-a" {
		t.Fatalf("repoIntegrationFor = (%q, %v), want ri-a — the watch-only row carries no launch policy and no usable connection", ri.ID, ok)
	}
	conn, ok := s.forgeConnectionForPR(ctx, "t1", "", "github.com", "SocialGouv/x")
	if !ok || conn.ID != "conn-a" {
		t.Fatalf("forgeConnectionForPR = (%q, %v), want conn-a (the older integration that covers the repo), not the host-wide latest", conn.ID, ok)
	}
}

// The same walk pins a repo-bound schedule to an integration id for its
// lifecycle (DeleteByIntegration). A watch-only row is the wrong anchor:
// de-provisioning the vulnerability watch would then delete the schedule.
func TestResolveRepoIntegrationID_SkipsWatchOnlyRows(t *testing.T) {
	s := newForgeTestServer(t)
	ctx := context.Background()
	now := time.Now()
	for _, c := range []forge.Connection{
		{ID: "conn-watch", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp, Status: forge.StatusActive, ForgeBaseURL: "https://github.com", Purpose: forge.PurposeSecurityRead, CreatedAt: now.Add(-72 * time.Hour)},
		{ID: "conn-a", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp, Status: forge.StatusActive, ForgeBaseURL: "https://github.com", Purpose: forge.PurposeRuntime, CreatedAt: now.Add(-1 * time.Hour)},
	} {
		if err := s.forgeConnections.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// The watch-only row is the OLDER one, so a store listing ascending by
	// created_at walks it first.
	for _, ri := range []forge.RepoIntegration{
		{ID: "ri-watch", TenantID: "t1", ConnectionID: "conn-watch", RepoFullName: "SocialGouv/x", CreatedAt: now.Add(-72 * time.Hour)},
		{ID: "ri-a", TenantID: "t1", ConnectionID: "conn-a", RepoFullName: "SocialGouv/x", CreatedAt: now.Add(-1 * time.Hour)},
	} {
		if err := s.forgeIntegrations.Create(ctx, ri); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.resolveRepoIntegrationID(ctx, "t1", "https://github.com/SocialGouv/x"); got != "ri-a" {
		t.Fatalf("resolveRepoIntegrationID = %q, want ri-a — a schedule anchored on the watch-only row dies with the vulnerability watch", got)
	}
}

// A repo provisioned twice on one host (re-provisioned onto a newer
// connection, the older integration left behind) must resolve to the LATEST
// provisioning's connection — the same choice repoIntegrationFor makes for
// the policy, so the verdict, the pending claim and the approve all post
// under the connection the operator currently intends.
func TestForgeConnectionForPR_IntegrationPicksLatestProvisioning(t *testing.T) {
	s := newForgeTestServer(t)
	ctx := context.Background()
	for _, c := range []forge.Connection{
		{ID: "conn-older", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp, Status: forge.StatusActive, ForgeBaseURL: "https://github.com", Purpose: forge.PurposeRuntime, CreatedAt: time.Now().Add(-48 * time.Hour)},
		{ID: "conn-newer", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp, Status: forge.StatusActive, ForgeBaseURL: "https://github.com", Purpose: forge.PurposeRuntime, CreatedAt: time.Now().Add(-1 * time.Hour)},
	} {
		if err := s.forgeConnections.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, ri := range []forge.RepoIntegration{
		{ID: "ri-older", TenantID: "t1", ConnectionID: "conn-older", RepoFullName: "SocialGouv/x", CreatedAt: time.Now().Add(-48 * time.Hour)},
		{ID: "ri-newer", TenantID: "t1", ConnectionID: "conn-newer", RepoFullName: "SocialGouv/x", CreatedAt: time.Now().Add(-1 * time.Hour)},
	} {
		if err := s.forgeIntegrations.Create(ctx, ri); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := s.forgeConnectionForPR(ctx, "t1", "", "github.com", "SocialGouv/x")
	if !ok {
		t.Fatal("resolver returned no match")
	}
	if got.ID != "conn-newer" {
		t.Fatalf("integration branch picked %q, want conn-newer (the latest provisioning) — publish, pending and approve all read through this", got.ID)
	}
}
