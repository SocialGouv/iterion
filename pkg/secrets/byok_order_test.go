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
		real, err := Resolve(t.Context(), st, "t", "alice", []Provider{ProviderAnthropic}, tc.pins, sealer, func(k ApiKey) bool { return k.ID != primary.ID })
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
