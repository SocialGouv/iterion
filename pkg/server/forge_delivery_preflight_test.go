package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestForgeDeliveryPreflightUsesActualTokenProof(t *testing.T) {
	for _, name := range []string{"allowed", "narrow-token-wide-installation", "unverified", "expired", "different-mounted-token", "stale-proof", "secret-other-tenant", "wrong-repo", "wrong-host", "wrong-team", "unknown-grant", "inactive"} {
		t.Run(name, func(t *testing.T) {
			s, review := newForgePublishTestServer(t)
			s.sealer, _ = secrets.NewAESGCMSealer(make([]byte, 32))
			s.genericSecrets = secrets.NewMemoryGenericSecretStore()
			ctx := store.WithTenant(t.Context(), "team1")
			conn, _ := s.forgeConnections.Get(ctx, "conn1")
			conn.Kind, conn.Status, conn.ManagedSecretID = forge.KindGitHubApp, forge.StatusActive, "managed"
			conn.GrantedPermissions = map[string]string{"contents": "write", "workflows": "write"}
			if name == "inactive" {
				conn.Status = forge.StatusNeedsReauth
			}
			if err := s.forgeConnections.Update(ctx, conn); err != nil {
				t.Fatal(err)
			}
			proof := secrets.NewTokenPermissionProof("ghs_runtime_secret", map[string]string{"contents": "write", "workflows": "write"}, time.Now().Add(time.Hour))
			switch name {
			case "narrow-token-wide-installation":
				delete(proof.Permissions, "workflows")
			case "unverified":
				proof = nil
			case "expired":
				proof.ExpiresAt = time.Now().Add(-time.Second)
			case "stale-proof":
				proof.SHA256 = secrets.TokenSHA256("previous-token")
			}
			sealed, _ := secrets.SealGenericSecret(s.sealer, "managed", []byte("ghs_runtime_secret"))
			rec := secrets.GenericSecret{ID: "managed", TenantID: "team1", ScopeTeamID: "team1", SealedSecret: sealed, ForgeTokenProof: proof}
			createCtx := ctx
			if name == "secret-other-tenant" {
				createCtx = store.WithTenant(t.Context(), "team2")
				rec.TenantID = "team2"
			}
			if err := s.genericSecrets.Create(createCtx, rec); err != nil {
				t.Fatal(err)
			}
			grant := ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"}
			if name == "wrong-team" {
				grant.TeamID = "team2"
			}
			registerPublishToken(t, s, "grant", grant)
			prURL, tokenHash := "https://github.com/o/r/pull/1", secrets.TokenSHA256("ghs_runtime_secret")
			if name == "different-mounted-token" {
				tokenHash = secrets.TokenSHA256("some-other-token")
			}
			if name == "wrong-repo" {
				prURL = "https://github.com/o/other/pull/1"
			}
			if name == "wrong-host" {
				prURL = "https://other.example/o/r/pull/1"
			}
			body, _ := json.Marshal(map[string]string{"pr_url": prURL, "credential_sha256": tokenHash})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/forge/delivery-preflight", bytes.NewReader(body))
			r.Header.Set("X-Iterion-Run", "grant")
			if name == "unknown-grant" {
				r.Header.Set("X-Iterion-Run", "unknown")
			}
			w := httptest.NewRecorder()
			s.handleForgeDeliveryPreflight(w, r)
			if strings.Contains(w.Body.String(), "ghs_runtime_secret") || review.calls != 0 {
				t.Fatal("preflight leaked a secret or wrote to the forge")
			}
			if name == "wrong-repo" || name == "wrong-host" || name == "wrong-team" || name == "unknown-grant" {
				if w.Code < 400 {
					t.Fatalf("scope bypass: %d %s", w.Code, w.Body)
				}
				return
			}
			var verdict forgeDeliveryVerdict
			if err := json.Unmarshal(w.Body.Bytes(), &verdict); err != nil {
				t.Fatalf("response: %d %s", w.Code, w.Body)
			}
			if verdict.OK != (name == "allowed") {
				t.Fatalf("verdict=%+v", verdict)
			}
			if !verdict.OK && (verdict.Code != "FORGE_PERMISSION_DENIED" || !strings.Contains(verdict.Reason, "workflows")) {
				t.Fatalf("untyped refusal: %+v", verdict)
			}
		})
	}
}
