package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

type unavailableEpochStore struct{ forge.RepoIntegrationStore }

func (unavailableEpochStore) Get(context.Context, string) (forge.RepoIntegration, error) {
	return forge.RepoIntegration{}, errors.New("store unavailable")
}

func TestWebhookEpochAdmissionUsesAuthoritativeIntegration(t *testing.T) {
	for _, state := range []string{"active", "pending", "retired", "successor", "unavailable", "unwired"} {
		t.Run(state, func(t *testing.T) {
			s := newWebhookTestServer(t)
			ctx := context.Background()
			secret, hash, _, _, err := webhooks.MintToken(testMintVerdictContext(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			cfg := webhooks.Config{ID: "epoch", TenantID: "t1", Provider: webhooks.ProviderGitLab, Enabled: true, TokenHash: hash, ProvisionIntegrationID: "integration"}
			if err := s.webhookConfigs.Create(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			is := forge.NewMemoryRepoIntegrationStore()
			ri := forge.RepoIntegration{ID: "integration", TenantID: "t1", ConnectionID: "conn", RepoFullName: "group/repo", WebhookID: cfg.ID}
			want := http.StatusNoContent
			switch state {
			case "pending":
				ri.WebhookID = ""
				ri.ProvisionPending = true
				want = http.StatusGone
			case "retired":
				ri.Retired = true
				want = http.StatusGone
			case "successor":
				ri.WebhookID = "new-epoch"
				want = http.StatusGone
			case "unavailable", "unwired":
				want = http.StatusServiceUnavailable
			}
			if err := is.Create(ctx, ri); err != nil {
				t.Fatal(err)
			}
			s.forgeIntegrations = is
			if state == "unavailable" {
				s.forgeIntegrations = unavailableEpochStore{is}
			}
			if state == "unwired" {
				s.forgeIntegrations = nil
			}
			called := false
			h := s.webhookAuth(webhooks.ProviderGitLab, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }))
			r := httptest.NewRequest("POST", "/api/webhooks/gitlab/epoch", nil)
			r.SetPathValue("id", cfg.ID)
			r.Header.Set("X-Gitlab-Token", secret)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != want || called != (state == "active") {
				t.Fatalf("epoch %s admission=%d called=%v: %s", state, w.Code, called, w.Body.String())
			}
		})
	}
}
