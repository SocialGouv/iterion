package secrets

import (
	"reflect"
	"testing"
	"time"
)

func TestOrderedAPIKeysMatchesResolveAndKeepsInput(t *testing.T) {
	st := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	team := mkKey(t, st, sealer, "t", "", ProviderAnthropic, "team-default", "team", true)
	other := mkKey(t, st, sealer, "t", "alice", ProviderAnthropic, "user-other", "other", false)
	primary := mkKey(t, st, sealer, "t", "alice", ProviderAnthropic, "user-default", "primary", true)
	// The earlier creation time never lets a lower scope beat a higher one.
	team.CreatedAt = time.Unix(1, 0)
	rows := []ApiKey{team, other, primary}
	before := append([]ApiKey(nil), rows...)
	cases := []struct {
		pins map[Provider]string
		want []string
	}{{nil, []string{primary.ID, other.ID, team.ID}}, {map[Provider]string{ProviderAnthropic: team.ID}, []string{team.ID, primary.ID, other.ID, team.ID}}}
	for _, tc := range cases {
		ordered := OrderedAPIKeyCandidates(rows, "alice", []Provider{ProviderAnthropic}, tc.pins)
		ids := make([]string, len(ordered))
		for i, c := range ordered {
			ids[i] = c.Key.ID
		}
		if !reflect.DeepEqual(ids, tc.want) {
			t.Fatalf("order=%v want=%v", ids, tc.want)
		}
		if !reflect.DeepEqual(rows, before) {
			t.Fatal("ordering mutated caller metadata")
		}
		real, err := Resolve(t.Context(), st, "t", "alice", "", []Provider{ProviderAnthropic}, tc.pins, sealer, func(k ApiKey) bool { return k.ID != primary.ID }, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := other.ID
		if tc.pins != nil {
			want = team.ID
		}
		if real[ProviderAnthropic].KeyID != want {
			t.Fatalf("actual selection=%s want=%s", real[ProviderAnthropic].KeyID, want)
		}
	}
}

// Two keys tied on rank AND created_at are ordered by id: a stable key, not
// the store's listing order (map order in memory, unspecified within a tie
// in Mongo). The winner is frozen into a run's content boundary KeyID and
// its stamp, so it must not depend on the order the rows arrive in — both
// orders yield the same sequence.
func TestOrderedAPIKeysBreaksAnExactTieOnTheID(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	a := ApiKey{ID: "key-a", TenantID: "t", ScopeTeamID: "t", Provider: ProviderOpenAICompatible, CreatedAt: at}
	b := ApiKey{ID: "key-b", TenantID: "t", ScopeTeamID: "t", Provider: ProviderOpenAICompatible, CreatedAt: at}
	for _, rows := range [][]ApiKey{{a, b}, {b, a}} {
		ordered := OrderedAPIKeyCandidates(rows, "", []Provider{ProviderOpenAICompatible}, nil)
		ids := make([]string, len(ordered))
		for i, c := range ordered {
			ids[i] = c.Key.ID
		}
		if !reflect.DeepEqual(ids, []string{"key-a", "key-b"}) {
			t.Fatalf("input %s,%s ordered %v, want [key-a key-b] whatever the listing order", rows[0].ID, rows[1].ID, ids)
		}
	}
}
