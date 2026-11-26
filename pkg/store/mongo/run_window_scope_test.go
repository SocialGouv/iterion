package mongo

import (
	"context"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// RunWindow.TenantIDs narrows; it never widens the tenant scope the context
// imposes. A team-scoped caller naming another team, or the tenant-less runs,
// gets nothing; naming its own team among others gets its own runs only; and
// each team's scope answers that team's runs and no other's.
func TestListRunWindow_TenantSetNeverWidensTheContextScope(t *testing.T) {
	s := watchersTestStore(t)
	ctx := context.Background()
	teamA := store.WithIdentity(ctx, "team-a", "owner")
	teamB := store.WithIdentity(ctx, "team-b", "owner")
	blind := store.TeamBlind(ctx)
	for _, c := range []struct {
		ctx context.Context
		id  string
	}{{teamA, "a1"}, {teamA, "a2"}, {teamB, "b1"}, {teamB, "b2"}, {blind, "none1"}} {
		if _, err := s.CreateRun(c.ctx, c.id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", c.id, err)
		}
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		set  []string
		want []string
	}{
		{"team-a names team-b", teamA, []string{"team-b"}, nil},
		{"team-a names the tenant-less runs", teamA, []string{""}, nil},
		{"team-a names team-b and itself", teamA, []string{"team-b", "team-a"}, []string{"a1", "a2"}},
		{"team-a, no set", teamA, nil, []string{"a1", "a2"}},
		{"team-b, no set", teamB, nil, []string{"b1", "b2"}},
		{"team-b names team-a", teamB, []string{"team-a"}, nil},
		{"blind names the tenant-less runs", blind, []string{""}, []string{"none1"}},
		{"blind names team-b", blind, []string{"team-b"}, []string{"b1", "b2"}},
	} {
		runs, skipped, err := s.ListRunWindow(tc.ctx, store.RunWindow{TenantIDs: tc.set})
		if err != nil || len(skipped) != 0 {
			t.Fatalf("%s: ListRunWindow: %v, skipped %v", tc.name, err, skipped)
		}
		var got []string
		for _, r := range runs {
			got = append(got, r.ID)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: listed %v, want %v — a tenant set must narrow the context's scope, never replace it", tc.name, got, tc.want)
		}
	}
}
