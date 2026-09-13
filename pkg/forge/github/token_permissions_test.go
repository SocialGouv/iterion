package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMintProofUsesReturnedPermissionsNotRequestedGrant(t *testing.T) {
	key, _ := testKeyPEM(t)
	now := time.Now()
	for _, returned := range []map[string]string{nil, {"contents": "write"}, {"contents": "write", "workflows": "read"}, {"contents": "write", "workflows": "write"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_actual", "expires_at": now.Add(time.Hour).Format(time.RFC3339), "permissions": returned})
		}))
		out, err := MintInstallationTokenWithPermissions(t.Context(), srv.Client(), srv.URL, AppConfig{AppID: 42, PrivateKeyPEM: key}, 1, now,
			&InstallationTokenOptions{Permissions: map[string]string{"contents": "write", "workflows": "write"}})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := out.TokenProof.Allows(out.AccessToken, "workflows", now), returned["workflows"] == "write"; got != want {
			t.Fatalf("returned=%v allowed=%v want=%v", returned, got, want)
		}
		if out.TokenProof.Allows("different-token", "workflows", now) {
			t.Fatal("proof transferred to another token")
		}
	}
}
