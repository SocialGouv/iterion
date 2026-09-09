package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// A stored App key that is not parseable PEM fails while the JWT is being
// signed — before a socket is opened. The assertion that matters is not the
// error text but that the forge was never contacted AND that the error says
// so: every handler downstream defaults to 502, so an unmarked failure here
// reports GitHub as broken for a key only iterion can read.
func TestMintInstallationToken_UnparseableKeyNeverReachesTheForge(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	cfg := AppConfig{AppID: 42, PrivateKeyPEM: "-----BEGIN RSA PRIVATE KEY-----\nnot base64\n-----END RSA PRIVATE KEY-----", AppSlug: "iterion"}
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0), nil)
	if err == nil {
		t.Fatal("an unparseable private key minted a token")
	}
	if reached {
		t.Fatal("the mint opened a socket with a key it could not read — the pre-flight claim this marker rests on is false")
	}
	if !errors.Is(err, forge.ErrLocalPreflight) {
		t.Fatalf("mint error %v carries no forge.ErrLocalPreflight — a 502-defaulting handler will blame the forge for a key iterion stored", err)
	}
}

// The other direction, which is the way this fix could itself become a lie:
// a genuine refusal from the forge must NOT be marked, or a GitHub outage
// would be reported as iterion's own fault.
func TestMintInstallationToken_ForgeRefusalIsNotMarkedLocal(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message":"upstream is down"}`))
	}))
	defer srv.Close()

	cfg := AppConfig{AppID: 42, PrivateKeyPEM: pemStr, AppSlug: "iterion"}
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0), nil)
	if err == nil {
		t.Fatal("a 502 from the forge minted a token")
	}
	if errors.Is(err, forge.ErrLocalPreflight) {
		t.Fatalf("a forge 5xx was marked as iterion's own (%v) — the same inversion, reversed", err)
	}
}
