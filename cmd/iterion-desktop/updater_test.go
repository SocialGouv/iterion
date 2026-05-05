//go:build desktop

package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
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
