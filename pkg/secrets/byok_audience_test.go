package secrets

import "testing"

// mkAudienceKey stores a key and gives it a workload audience. Separate from
// mkKey so the audience is the only thing these fixtures vary.
func mkAudienceKey(t *testing.T, st *MemoryApiKeyStore, sealer Sealer, name string, bots []string) ApiKey {
	t.Helper()
	k := mkKey(t, st, sealer, "t", "", ProviderAnthropic, name, "secret-"+name, false)
	k.Bots = bots
	if err := st.Update(t.Context(), k); err != nil {
		t.Fatalf("update %s: %v", name, err)
	}
	return k
}

// resolvedID names the key that funded the anthropic slot, or "" when the
// walk found none.
func resolvedID(t *testing.T, st *MemoryApiKeyStore, sealer Sealer, botID string, pins map[Provider]string, usable func(ApiKey) bool) string {
	t.Helper()
	out, err := Resolve(t.Context(), st, "t", "", botID, []Provider{ProviderAnthropic}, pins, sealer, usable, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	r, ok := out[ProviderAnthropic]
	if !ok {
		return ""
	}
	return r.KeyID
}

// A key that names its workload must not fund a different one. The fixture
// deliberately leaves NO other anthropic key: the audience has to be what
// empties the slot, not the presence of a better candidate.
func TestAKeyNamingItsBotDoesNotFundAnother(t *testing.T) {
	st, sealer := NewMemoryApiKeyStore(), newSealer(t)
	scoped := mkAudienceKey(t, st, sealer, "scoped", []string{"sec-audit-source"})

	if got := resolvedID(t, st, sealer, "review-pr", nil, nil); got != "" {
		t.Fatalf("a key scoped to sec-audit-source funded review-pr (key %s)", got)
	}
	if got := resolvedID(t, st, sealer, "sec-audit-source", nil, nil); got != scoped.ID {
		t.Fatalf("the named bot was not served: got %q want %q", got, scoped.ID)
	}
}

// The field is opt-in: a key written before it existed, or one an operator
// never scoped, keeps funding everything. Without this, adding the column
// would be a migration that silently defunds a fleet.
func TestAnEmptyAudienceStillFundsEveryBot(t *testing.T) {
	st, sealer := NewMemoryApiKeyStore(), newSealer(t)
	open := mkAudienceKey(t, st, sealer, "open", nil)

	for _, bot := range []string{"review-pr", "sec-audit-source", ""} {
		if got := resolvedID(t, st, sealer, bot, nil, nil); got != open.ID {
			t.Fatalf("bot %q: got %q want %q", bot, got, open.ID)
		}
	}
}

// An inline `.bot` a requester uploaded carries no bot id. Treating that as
// "no bot named, so the allow-list does not apply" would hand a scoped key
// to any file someone submits — the one input the requester fully controls
// has to fail closed, exactly as credpool.Pledge.AvailableForLaunch does.
func TestAnAudienceRefusesARunThatNamesNoBot(t *testing.T) {
	st, sealer := NewMemoryApiKeyStore(), newSealer(t)
	mkAudienceKey(t, st, sealer, "scoped", []string{"sec-audit-source"})

	if got := resolvedID(t, st, sealer, "", nil, nil); got != "" {
		t.Fatalf("a scoped key funded a run that names no bot (key %s)", got)
	}
}

// The audience is a GATE, so a pin must not lift it. A pin is honoured over
// the usable-predicate by design — that is what keeps the predicate an
// optimisation — and reading the audience through the same hook would have
// made "pin the key" the documented way around it.
func TestAPinDoesNotLiftAnAudience(t *testing.T) {
	st, sealer := NewMemoryApiKeyStore(), newSealer(t)
	scoped := mkAudienceKey(t, st, sealer, "scoped", []string{"sec-audit-source"})
	pins := map[Provider]string{ProviderAnthropic: scoped.ID}

	if got := resolvedID(t, st, sealer, "review-pr", pins, nil); got != "" {
		t.Fatalf("pinning lifted the audience (key %s)", got)
	}
	if got := resolvedID(t, st, sealer, "sec-audit-source", pins, nil); got != scoped.ID {
		t.Fatalf("pinning the named bot's own key stopped working: got %q", got)
	}
}

// The refused-key restore re-resolves with a NIL predicate so a run never
// publishes with an empty wire. That path is why the audience cannot live in
// the predicate: expressed there, it would be lifted on exactly the walk it
// exists to close. This reproduces the restore's shape — nil predicate, no
// other candidate — and requires the slot to stay empty.
func TestTheRefusedKeyRestoreDoesNotLiftAnAudience(t *testing.T) {
	st, sealer := NewMemoryApiKeyStore(), newSealer(t)
	scoped := mkAudienceKey(t, st, sealer, "scoped", []string{"sec-audit-source"})

	// First the shape the live walk has: a predicate that refuses everything,
	// standing in for a provider that just turned the key away.
	if got := resolvedID(t, st, sealer, "review-pr", nil, func(ApiKey) bool { return false }); got != "" {
		t.Fatalf("live walk served a scoped key (key %s)", got)
	}
	// Then the restore: same team, same providers, predicate dropped.
	if got := resolvedID(t, st, sealer, "review-pr", nil, nil); got != "" {
		t.Fatalf("the restore served a key scoped to another bot (key %s)", got)
	}
	// And the restore must still work for the bot the key names, or the
	// test above would pass on a resolver that simply stopped restoring.
	if got := resolvedID(t, st, sealer, "sec-audit-source", nil, nil); got != scoped.ID {
		t.Fatalf("the restore stopped serving the named bot: got %q", got)
	}
}

// An audience narrows WHICH key serves, never WHETHER the walk continues: a
// second key of the same provider behind the scoped one must take the slot.
// Without this, refusing a key and emptying the provider look identical.
func TestAScopedKeyStepsAsideForTheNextKey(t *testing.T) {
	st, sealer := NewMemoryApiKeyStore(), newSealer(t)
	scoped := mkAudienceKey(t, st, sealer, "scoped", []string{"sec-audit-source"})
	open := mkAudienceKey(t, st, sealer, "open", nil)

	if got := resolvedID(t, st, sealer, "review-pr", nil, nil); got != open.ID {
		t.Fatalf("the walk did not fall through to the unscoped key: got %q want %q", got, open.ID)
	}
	if got := resolvedID(t, st, sealer, "sec-audit-source", nil, nil); got != scoped.ID {
		t.Fatalf("the scoped key lost its own bot to the unscoped one: got %q want %q", got, scoped.ID)
	}
}

// The predicate matches EXACTLY, and every row that is false here is a way an
// audience could be silently widened. The first table of this test used two
// bot ids with no shared prefix and no case variance: it proved membership and
// nothing else, and two widening mutations — a prefix match, and a
// case-insensitive one — stayed green under it. Both are the change a
// maintainer "harmonising" this with the repo's tolerant bot-name lookup would
// make, so each now has its row.
//
// Canonicalisation is a separate mechanism, tested at the edges that apply it
// (normalizeBotAudience and the publisher's single fold). It is deliberately
// NOT done here: two spelling rules competing inside one comparison is how the
// tolerant and the exact reading drift apart.
func TestTheAudiencePredicateMatchesExactlyAndNothingNear(t *testing.T) {
	scoped := ApiKey{Bots: []string{"sec-audit-source"}}
	open := ApiKey{}

	cases := []struct {
		name  string
		key   ApiKey
		botID string
		want  bool
	}{
		{"the named bot", scoped, "sec-audit-source", true},
		{"an unrelated bot", scoped, "review-pr", false},
		{"no bot id at all", scoped, "", false},
		{"a PREFIX of the named bot", scoped, "sec-audit", false},
		{"a one-character prefix", scoped, "s", false},
		{"the named bot with a suffix", scoped, "sec-audit-source-v2", false},
		{"a case variant", scoped, "Sec-Audit-Source", false},
		{"an upper-case variant", scoped, "SEC-AUDIT-SOURCE", false},
		{"an underscore variant", scoped, "sec_audit_source", false},
		{"surrounding whitespace", scoped, " sec-audit-source ", false},
		{"open key, any bot", open, "review-pr", true},
		{"open key, no bot id", open, "", true},
	}
	for _, tc := range cases {
		if got := tc.key.ServesBotForLaunch(tc.botID); got != tc.want {
			t.Errorf("%s: ServesBotForLaunch(%q)=%v want %v", tc.name, tc.botID, got, tc.want)
		}
	}
}

// The audience has to survive a round trip through the store, or it is a
// runtime-only guard that a restart lifts.
func TestAnAudienceSurvivesTheStore(t *testing.T) {
	st, sealer := NewMemoryApiKeyStore(), newSealer(t)
	scoped := mkAudienceKey(t, st, sealer, "scoped", []string{"sec-audit-source", "sec-audit-deps"})

	back, err := st.Get(t.Context(), scoped.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(back.Bots) != 2 || back.Bots[0] != "sec-audit-source" || back.Bots[1] != "sec-audit-deps" {
		t.Fatalf("audience did not round-trip: %v", back.Bots)
	}
	// And clearing it is expressible — an audience one can set but never lift
	// is a key an operator has to delete and recreate.
	back.Bots = nil
	if err := st.Update(t.Context(), back); err != nil {
		t.Fatalf("update: %v", err)
	}
	cleared, err := st.Get(t.Context(), scoped.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if len(cleared.Bots) != 0 {
		t.Fatalf("audience could not be cleared: %v", cleared.Bots)
	}
}
