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
