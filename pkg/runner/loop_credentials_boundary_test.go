package runner

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-131 §4, the runner's belt under the queue's schema rule: the message's
// team_gateway stamp IS the ambient refusal, whatever carriage lost the flag
// — a stale replica's old-rules bundle that sealed a vendor key and no flag,
// or no bundle at all (a gateway-only run whose old-rules walk sealed
// nothing). Without it the gateway routes of such a run fall through to the
// pod's OPENAI_COMPATIBLE_* environment. An unstamped or own_keys run reads
// its bundle's flag alone, and a run with neither a bundle nor a stamp keeps
// the passthrough (TestInjectCredentials_NoRefIsPassthrough).
func TestInjectCredentials_TheTeamGatewayStampRefusesTheAmbient(t *testing.T) {
	teamGateway := &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "k", GatewayHost: "gw.example"}

	t.Run("no bundle under team_gateway", func(t *testing.T) {
		r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
		ctx, cleanup, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-cb", TenantID: "team-a", ContentBoundary: teamGateway})
		if err != nil {
			t.Fatalf("injectCredentials: %v", err)
		}
		if cleanup != nil {
			defer cleanup()
		}
		creds, ok := secrets.CredentialsFromContext(ctx)
		if !ok || !creds.AmbientRefused {
			t.Fatalf("credentials on ctx = %v, ambient refused = %v — a team_gateway run with no bundle must refuse the pod's environment", ok, creds.AmbientRefused)
		}
	})

	for _, row := range []struct {
		name     string
		boundary *store.RunContentBoundary
		refused  bool
	}{
		{"an old-rules bundle without the flag under team_gateway", teamGateway, true},
		{"the same bundle on an own_keys run (the control)", &store.RunContentBoundary{Kind: store.ContentBoundaryOwnKeys}, false},
		{"the same bundle unstamped (the control)", nil, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			rs := secrets.NewMemoryRunSecretsStore()
			sealed, dek := sealDekFixture(t, "team-a", "run-cb", secrets.RunBundle{APIKeys: map[secrets.Provider]string{"anthropic": "sk-live-1"}})
			if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-cb", TenantID: "team-a", RunID: "run-cb", KeyID: secrets.DEKKeyID, SealedBundle: sealed}); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs}}
			ctx, cleanup, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-cb", TenantID: "team-a", SecretsRef: "ref-cb", BundleDEK: dek, ContentBoundary: row.boundary})
			if err != nil {
				t.Fatalf("injectCredentials: %v", err)
			}
			defer cleanup()
			creds, ok := secrets.CredentialsFromContext(ctx)
			if !ok {
				t.Fatal("no credentials on the context")
			}
			if creds.AmbientRefused != row.refused {
				t.Fatalf("ambient refused = %v, want %v", creds.AmbientRefused, row.refused)
			}
		})
	}
}
