package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// TestScheduledForgeOverrides pins the A23 fix: a repo-bound schedule mints
// its clone token from the pinned integration's connection at the tick —
// the same managed-secret path every studio/API launch uses — instead of
// leaning on a hand-set `forge_token` team secret that expires and then
// kills every tick at clone while manual launches keep working.
func TestScheduledForgeOverrides(t *testing.T) {
	newWorld := func(t *testing.T) (*Server, forge.Connection) {
		s := newForgeTestServer(t)
		sealed, err := forge.SealPAT(s.sealer, "c1", "glpat-token")
		if err != nil {
			t.Fatal(err)
		}
		conn := forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitLab, SealedPayload: sealed}
		if err := s.forgeConnections.Create(context.Background(), conn); err != nil {
			t.Fatal(err)
		}
		integ := forge.RepoIntegration{ID: "i1", TenantID: "t1", ConnectionID: "c1", RepoFullName: "group/api"}
		if err := s.forgeIntegrations.Create(context.Background(), integ); err != nil {
			t.Fatal(err)
		}
		return s, conn
	}

	t.Run("pinned integration mints the managed secret", func(t *testing.T) {
		s, _ := newWorld(t)
		ov, err := s.scheduledForgeOverrides(context.Background(), cloudsched.ScheduledBot{
			ID: "sch1", TenantID: "t1", BotID: "feed-watch", RepoIntegrationID: "i1",
		})
		if err != nil {
			t.Fatalf("scheduledForgeOverrides: %v", err)
		}
		secID := ov["forge_token"]
		if secID == "" {
			t.Fatal("no forge_token override — the tick would fall back to the expiring hand-set secret")
		}
		// The mint is stamped on the connection so the refresh worker keeps
		// rotating the plaintext under the same id.
		conn, err := s.forgeConnections.Get(context.Background(), "c1")
		if err != nil || conn.ManagedSecretID != secID {
			t.Fatalf("managed secret not stamped on the connection: got %q want %q (%v)", conn.ManagedSecretID, secID, err)
		}
	})

	t.Run("no pinned integration keeps the legacy binding path", func(t *testing.T) {
		s, _ := newWorld(t)
		ov, err := s.scheduledForgeOverrides(context.Background(), cloudsched.ScheduledBot{
			ID: "sch2", TenantID: "t1", BotID: "feed-watch",
		})
		if err != nil || ov != nil {
			t.Fatalf("un-pinned schedule must resolve nothing, got %v (%v)", ov, err)
		}
	})

	t.Run("tenant mismatch is refused explicitly", func(t *testing.T) {
		s, _ := newWorld(t)
		_, err := s.scheduledForgeOverrides(context.Background(), cloudsched.ScheduledBot{
			ID: "sch3", TenantID: "t2", BotID: "feed-watch", RepoIntegrationID: "i1",
		})
		if err == nil || !strings.Contains(err.Error(), "another tenant") {
			t.Fatalf("cross-tenant integration must be refused, got %v", err)
		}
	})

	t.Run("a dangling integration id fails the tick loudly", func(t *testing.T) {
		s, _ := newWorld(t)
		_, err := s.scheduledForgeOverrides(context.Background(), cloudsched.ScheduledBot{
			ID: "sch4", TenantID: "t1", BotID: "feed-watch", RepoIntegrationID: "gone",
		})
		if err == nil {
			t.Fatal("a pinned-but-missing integration must fail the tick, not limp to a doomed clone")
		}
	})
}
