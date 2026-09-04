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

	got, err := Resolve(context.Background(), store, "team", "alice", []Provider{ProviderOpenAI}, nil, sealer, nil)
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

	got, _ := Resolve(context.Background(), store, "team", "bob", []Provider{ProviderOpenAI}, nil, sealer, nil)
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
		sealer, nil)
	r := got[ProviderOpenAI]
	if r.KeyID != other.ID || string(r.Plaintext) != "sk-other" {
		t.Fatalf("override should win: got %s, default was %s", r.KeyID, def.ID)
	}
}

func TestResolve_HidesOtherUsersKeys(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	mkKey(t, store, sealer, "team", "carol", ProviderOpenAI, "carol-only", "sk-carol", true)

	got, _ := Resolve(context.Background(), store, "team", "alice", []Provider{ProviderOpenAI}, nil, sealer, nil)
	if _, ok := got[ProviderOpenAI]; ok {
		t.Fatalf("alice should not see carol's user-scoped key")
	}
}

func TestResolve_OmitsProviderWhenNoKey(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	mkKey(t, store, sealer, "team", "", ProviderOpenAI, "team", "sk-t", true)

	got, _ := Resolve(context.Background(), store, "team", "alice",
		[]Provider{ProviderOpenAI, ProviderAnthropic}, nil, sealer, nil)
	if _, ok := got[ProviderAnthropic]; ok {
		t.Fatal("anthropic should be omitted (no key)")
	}
	if _, ok := got[ProviderOpenAI]; !ok {
		t.Fatal("openai should resolve")
	}
}

func TestProviderValid(t *testing.T) {
	valid := []Provider{
		ProviderAnthropic, ProviderOpenAI, ProviderBedrock, ProviderVertex,
		ProviderAzure, ProviderOpenRouter, ProviderXAI, ProviderZAI,
	}
	for _, p := range valid {
		if !p.Valid() {
			t.Errorf("Provider(%q).Valid() = false, want true", p)
		}
	}
	invalid := []Provider{"", "gpt", "Anthropic", "openai ", "google"}
	for _, p := range invalid {
		if p.Valid() {
			t.Errorf("Provider(%q).Valid() = true, want false", p)
		}
	}
}

func TestParseProvider(t *testing.T) {
	cases := []struct {
		in      string
		want    Provider
		wantErr bool
	}{
		{"openai", ProviderOpenAI, false},
		{"  OpenAI  ", ProviderOpenAI, false}, // trim + lowercase
		{"ZAI", ProviderZAI, false},
		{"ANTHROPIC", ProviderAnthropic, false},
		{"nope", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseProvider(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseProvider(%q) err = nil, want error", c.in)
				}
				if got != "" {
					t.Fatalf("ParseProvider(%q) provider = %q, want \"\" on error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseProvider(%q) unexpected err: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ParseProvider(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestKeyRank(t *testing.T) {
	const me = "alice"
	cases := []struct {
		name string
		key  ApiKey
		want int
	}{
		{"user default", ApiKey{ScopeUserID: me, IsDefault: true}, 0},
		{"user non-default", ApiKey{ScopeUserID: me}, 1},
		{"team default", ApiKey{ScopeUserID: "", IsDefault: true}, 2},
		{"team non-default", ApiKey{ScopeUserID: ""}, 3},
		{"other user's key never applies", ApiKey{ScopeUserID: "bob", IsDefault: true}, 99},
	}
	// cases are authored in descending priority (highest priority first).
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := keyRank(c.key, me); got != c.want {
				t.Fatalf("keyRank(%+v) = %d, want %d", c.key, got, c.want)
			}
		})
	}
	// The ranks must be strictly increasing in that priority order so Resolve
	// picks user-default > user > team-default > team, and never another
	// user's key. Combined with the per-case checks above, this proves
	// keyRank itself is strictly increasing across the priority chain.
	for i := 1; i < len(cases); i++ {
		if cases[i-1].want >= cases[i].want {
			t.Fatalf("priority order broken at %q -> %q: %d >= %d",
				cases[i-1].name, cases[i].name, cases[i-1].want, cases[i].want)
		}
	}
}

func TestResolve_UserNonDefaultBeatsTeamDefault(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	// Team has a default key; the user has only a NON-default key. The
	// user's key (rank 1) must still win over the team default (rank 2).
	mkKey(t, store, sealer, "team", "", ProviderOpenAI, "team-default", "sk-team", true)
	user := mkKey(t, store, sealer, "team", "alice", ProviderOpenAI, "alice-plain", "sk-alice", false)

	got, err := Resolve(context.Background(), store, "team", "alice", []Provider{ProviderOpenAI}, nil, sealer, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r := got[ProviderOpenAI]
	if r.KeyID != user.ID || string(r.Plaintext) != "sk-alice" || r.SourceScope != "user" {
		t.Fatalf("user non-default should beat team default, got %+v", r)
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

// The usable predicate turns several keys of one provider into an ordered
// fallback chain: a refused key is passed over and the walk yields the
// NEXT one. Measured need (2026-09-02): a provider froze one key's account
// and the resolver kept sealing it into every fresh run.
func TestResolve_UsablePredicateWalksToTheNextKey(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	first := mkKey(t, store, sealer, "team", "", ProviderZAI, "primary", "sk-zai-1", true)
	second := mkKey(t, store, sealer, "team", "", ProviderZAI, "backup", "sk-zai-2", false)

	refuse := map[string]bool{first.ID: true}
	got, err := Resolve(context.Background(), store, "team", "alice", []Provider{ProviderZAI}, nil, sealer,
		func(k ApiKey) bool { return !refuse[k.ID] })
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r := got[ProviderZAI]; r.KeyID != second.ID {
		t.Fatalf("expected the backup key after the primary was refused, got %+v", r)
	}

	// Every key refused: the provider is OMITTED, so the later credential
	// tiers get their turn — an empty slot beats a poisoned one.
	got, err = Resolve(context.Background(), store, "team", "alice", []Provider{ProviderZAI}, nil, sealer,
		func(ApiKey) bool { return false })
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, ok := got[ProviderZAI]; ok {
		t.Fatalf("expected no resolution when every key is refused, got %+v", got[ProviderZAI])
	}
}

// An explicit override pin is NOT filtered by the predicate: the operator
// named that key, and honouring the pin over the optimisation is what
// keeps the predicate an optimisation.
func TestResolve_UsablePredicateDoesNotFilterPins(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	pinned := mkKey(t, store, sealer, "team", "", ProviderZAI, "pinned", "sk-zai-pinned", false)

	got, err := Resolve(context.Background(), store, "team", "alice", []Provider{ProviderZAI},
		map[Provider]string{ProviderZAI: pinned.ID}, sealer,
		func(ApiKey) bool { return false })
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r := got[ProviderZAI]; r.KeyID != pinned.ID {
		t.Fatalf("expected the pinned key despite the predicate, got %+v", r)
	}
}


// MarkFingerprintUsed bumps last_used_at on every row whose stable audit
// identity matches — the seam the runner drives at metering time so a
// key currently serving is distinguishable from an idle one (#659 pt 2).
// The launch-grant-only MarkUsed(id, ...) never moved after admission,
// leaving the studio with a frozen last_used_at on keys actively being
// spent — measured live 2026-09-03.
func TestMemoryApiKey_MarkFingerprintUsed(t *testing.T) {
	store := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	k1 := mkKey(t, store, sealer, "team-a", "u1", ProviderAnthropic, "k1", "sk-ant-live", false)
	k2 := mkKey(t, store, sealer, "team-b", "u2", ProviderAnthropic, "k2", "sk-ant-other", false)

	if k1.LastUsedAt != nil || k2.LastUsedAt != nil {
		t.Fatalf("fresh keys must have no last_used_at: %v %v", k1.LastUsedAt, k2.LastUsedAt)
	}

	t.Run("empty fingerprint is a no-op", func(t *testing.T) {
		if err := store.MarkFingerprintUsed(context.Background(), "", time.Now()); err != nil {
			t.Fatalf("empty fp errored: %v", err)
		}
		if got, _ := store.Get(context.Background(), k1.ID); got.LastUsedAt != nil {
			t.Fatalf("empty fp bumped a row: %v", got.LastUsedAt)
		}
	})

	t.Run("unknown fingerprint is a no-op", func(t *testing.T) {
		if err := store.MarkFingerprintUsed(context.Background(), "no-such-fp", time.Now()); err != nil {
			t.Fatalf("unknown fp errored: %v", err)
		}
	})

	t.Run("matching fingerprint bumps THAT key, not the sibling", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Second)
		if err := store.MarkFingerprintUsed(context.Background(), k1.Fingerprint, now); err != nil {
			t.Fatalf("mark: %v", err)
		}
		got1, _ := store.Get(context.Background(), k1.ID)
		got2, _ := store.Get(context.Background(), k2.ID)
		if got1.LastUsedAt == nil || !got1.LastUsedAt.Equal(now) {
			t.Fatalf("k1.last_used_at = %v, want %v (the metering bump did not land)", got1.LastUsedAt, now)
		}
		if got2.LastUsedAt != nil {
			t.Fatalf("k2.last_used_at = %v, want nil (only k1's fingerprint was metered)", got2.LastUsedAt)
		}
	})
}
