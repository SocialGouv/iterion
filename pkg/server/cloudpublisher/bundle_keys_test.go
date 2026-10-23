package cloudpublisher

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The chokepoint writes a key with what travels with it — its gateway root
// in the matching URL map, the server's fingerprint — into the channel
// asked for, never creates an entry it is not funding, and takes the URL
// away with the key so a stale root never pairs with a later key.
func TestSetAPIKey_WritesTheKeyWithItsURLAndFingerprint(t *testing.T) {
	gw := secrets.ProviderOpenAICompatible
	var b secrets.RunBundle
	setAPIKey(&b, gw, "", "https://gw.example.test", "fp", false)
	if len(b.APIKeys) != 0 || len(b.APIBaseURLs) != 0 || len(b.APIKeyFingerprints) != 0 {
		t.Fatalf("an empty key wrote an entry: %+v", b)
	}
	setAPIKey(&b, gw, "sk-gw", "https://gw.example.test", "fp-gw", false)
	setAPIKey(&b, secrets.ProviderAnthropic, "sk-ant", "", "fp-ant", false)
	setAPIKey(&b, gw, "sk-gw-pinned", "https://gw2.example.test", "fp-gw-pinned", true)
	if b.APIKeys[gw] != "sk-gw" || b.APIBaseURLs[gw] != "https://gw.example.test" || b.APIKeys[secrets.ProviderAnthropic] != "sk-ant" || len(b.APIBaseURLs) != 1 {
		t.Fatalf("default channel: %+v", b)
	}
	if b.PinnedAPIKeys[gw] != "sk-gw-pinned" || b.PinnedAPIBaseURLs[gw] != "https://gw2.example.test" {
		t.Fatalf("pinned channel: %+v", b)
	}
	// The server's fingerprint rides with a gateway key only (a vendor key's
	// is the runner's recompute), and the first channel to hold the key
	// keeps it: the pinned write did not overwrite the default's.
	if b.APIKeyFingerprints[gw] != "fp-gw" || len(b.APIKeyFingerprints) != 1 {
		t.Fatalf("fingerprints: %+v, want the gateway key's own only", b.APIKeyFingerprints)
	}
	// A key re-written without a URL takes the stale URL away.
	setAPIKey(&b, gw, "sk-gw-rotated", "", "", false)
	if _, stale := b.APIBaseURLs[gw]; stale {
		t.Fatalf("a stale URL survived a key written without one: %+v", b.APIBaseURLs)
	}
	// The seal guard reads the pair the chokepoint wrote: a vendor key with
	// no URL seals, a gateway key with its URL seals.
	var ok secrets.RunBundle
	setAPIKey(&ok, gw, "sk-gw", "https://gw.example.test", "fp", false)
	setAPIKey(&ok, secrets.ProviderAnthropic, "sk-ant", "", "", false)
	sealer, _ := secrets.NewKeyRingSealer(map[string][]byte{"k": make([]byte, 32)}, "k")
	if _, _, err := secrets.SealRunBundle(sealer, "t", "", "r", ok); err != nil {
		t.Fatalf("a bundle the chokepoint wrote does not seal: %v", err)
	}
}

// clearAPIKey takes a channel's key and URL away together, and the
// fingerprint only when no channel holds a key for the provider any more.
func TestClearAPIKey_RemovesTheURLWithTheKey(t *testing.T) {
	gw := secrets.ProviderOpenAICompatible
	var b secrets.RunBundle
	setAPIKey(&b, gw, "sk-own", "https://own.example.test", "fp-own", false)
	setAPIKey(&b, gw, "sk-pinned", "https://pinned.example.test", "fp-pinned", true)
	if b.APIKeyFingerprints[gw] != "fp-own" {
		t.Fatalf("the pinned write overwrote the default channel's fingerprint: %+v", b.APIKeyFingerprints)
	}
	clearAPIKey(&b, gw, true)
	if _, k := b.PinnedAPIKeys[gw]; k {
		t.Fatal("the pinned key survived")
	}
	if _, u := b.PinnedAPIBaseURLs[gw]; u {
		t.Fatal("the pinned URL survived its key")
	}
	if b.APIKeyFingerprints[gw] == "" {
		t.Fatal("the fingerprint went while the default channel still holds a key")
	}
	clearAPIKey(&b, gw, false)
	if len(b.APIKeys) != 0 || len(b.APIBaseURLs) != 0 || len(b.APIKeyFingerprints) != 0 {
		t.Fatalf("the default channel did not clear with its URL and fingerprint: %+v", b)
	}
}
