//go:build desktop

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUpdaterPublicKeyConfigured(t *testing.T) {
	pk, err := hex.DecodeString(updaterPublicKeyHex)
	if err != nil {
		t.Fatalf("updater public key is not valid hex: %v", err)
	}
	if len(pk) != ed25519.PublicKeySize {
		t.Fatalf("updater public key length = %d bytes, want %d", len(pk), ed25519.PublicKeySize)
	}
	var allZero = true
	for _, b := range pk {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("updater public key must not be the all-zero placeholder")
	}
}

// TestCheckForUpdate_404IsNoUpdate ensures the auto-updater treats a 404
// on the manifest URL as "no release published yet" rather than a hard
// error. The desktop UI's "Check for updates" button calls this directly
// and would otherwise surface a confusing "404 Not Found" string to users
// who simply haven't shipped a desktop release on their fork.
func TestCheckForUpdate_404IsNoUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("ITERION_UPDATE_MANIFEST_URL", srv.URL+"/manifest.json")

	cfg := NewConfig()
	u := NewUpdater(cfg)
	u.client.Timeout = 2 * time.Second

	rel, err := u.CheckForUpdate(context.Background(), ChannelStable)
	if err != nil {
		t.Fatalf("CheckForUpdate returned error %v on 404, want nil (no update available)", err)
	}
	if rel != nil {
		t.Fatalf("CheckForUpdate returned %+v on 404, want nil", rel)
	}
}
