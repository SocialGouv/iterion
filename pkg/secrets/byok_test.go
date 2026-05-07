package secrets

import (
	"context"
	"crypto/rand"
	"testing"
	"time"
)

func newSealer(t *testing.T) *AESGCMSealer {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	s, err := NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

func mkKey(t *testing.T, store *MemoryApiKeyStore, sealer Sealer, team, user string, p Provider, name, secret string, def bool) ApiKey {
	t.Helper()
	id := NewApiKeyID()
	sealed, err := SealAPIKey(sealer, id, []byte(secret))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	k := ApiKey{
		ID:           id,
		ScopeTeamID:  team,
		ScopeUserID:  user,
		Provider:     p,
		Name:         name,
		Last4:        Last4(secret),
		SealedSecret: sealed,
		IsDefault:    def,
		CreatedAt:    time.Now().UTC(),
		Fingerprint:  FingerprintSHA256(secret),
	}
	if err := store.Create(context.Background(), k); err != nil {
		t.Fatalf("create: %v", err)
	}
	return k
}

func TestResolve_PrioritizesUserOverTeam(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)

	mkKey(t, store, sealer, "team", "", ProviderOpenAI, "team-default", "sk-team-default", true)
	user := mkKey(t, store, sealer, "team", "alice", ProviderOpenAI, "alice-default", "sk-alice-default", true)

	got, err := Resolve(context.Background(), store, "team", "alice", []Provider{ProviderOpenAI}, nil, sealer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r, ok := got[ProviderOpenAI]
	if !ok {
		t.Fatalf("no resolution for openai")
	}
	if r.KeyID != user.ID || string(r.Plaintext) != "sk-alice-default" || r.SourceScope != "user" {
		t.Fatalf("expected user default, got %+v", r)
	}
}

func TestResolve_FallsBackToTeam(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	team := mkKey(t, store, sealer, "team", "", ProviderOpenAI, "team-default", "sk-team-default", true)

	got, _ := Resolve(context.Background(), store, "team", "bob", []Provider{ProviderOpenAI}, nil, sealer)
	r := got[ProviderOpenAI]
	if r.KeyID != team.ID || string(r.Plaintext) != "sk-team-default" || r.SourceScope != "team" {
		t.Fatalf("expected team default, got %+v", r)
	}
}

func TestResolve_OverrideWins(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	def := mkKey(t, store, sealer, "team", "alice", ProviderOpenAI, "default", "sk-def", true)
	other := mkKey(t, store, sealer, "team", "alice", ProviderOpenAI, "other", "sk-other", false)

	got, _ := Resolve(context.Background(), store, "team", "alice",
		[]Provider{ProviderOpenAI},
		map[Provider]string{ProviderOpenAI: other.ID},
		sealer)
	r := got[ProviderOpenAI]
	if r.KeyID != other.ID || string(r.Plaintext) != "sk-other" {
		t.Fatalf("override should win: got %s, default was %s", r.KeyID, def.ID)
	}
}

func TestResolve_HidesOtherUsersKeys(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	mkKey(t, store, sealer, "team", "carol", ProviderOpenAI, "carol-only", "sk-carol", true)

	got, _ := Resolve(context.Background(), store, "team", "alice", []Provider{ProviderOpenAI}, nil, sealer)
	if _, ok := got[ProviderOpenAI]; ok {
		t.Fatalf("alice should not see carol's user-scoped key")
	}
}

func TestResolve_OmitsProviderWhenNoKey(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	mkKey(t, store, sealer, "team", "", ProviderOpenAI, "team", "sk-t", true)

	got, _ := Resolve(context.Background(), store, "team", "alice",
		[]Provider{ProviderOpenAI, ProviderAnthropic}, nil, sealer)
	if _, ok := got[ProviderAnthropic]; ok {
		t.Fatal("anthropic should be omitted (no key)")
	}
	if _, ok := got[ProviderOpenAI]; !ok {
		t.Fatal("openai should resolve")
	}
}

func TestSealRunBundleRoundTrip(t *testing.T) {
	sealer := newSealer(t)
	bundle := RunBundle{
		APIKeys: map[Provider]string{
			ProviderOpenAI:    "sk-test",
			ProviderAnthropic: "sk-ant-test",
		},
	}
	sealed, err := SealRunBundle(sealer, "run-123", bundle)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := OpenRunBundle(sealer, "run-123", sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got.APIKeys[ProviderOpenAI] != "sk-test" || got.APIKeys[ProviderAnthropic] != "sk-ant-test" {
		t.Fatalf("roundtrip lost data: %+v", got)
	}
	// AAD pinning: opening with a different run id must fail.
	if _, err := OpenRunBundle(sealer, "run-999", sealed); err == nil {
		t.Fatal("expected AAD mismatch failure when run id changes")
	}
}
