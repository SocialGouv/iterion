package server

import (
	"context"
	"testing"

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
