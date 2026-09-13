package assistantmission

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMissionReconcileCandidates(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) Store
	}{
		{"fs", func(t *testing.T) Store { return NewFSStore(t.TempDir()) }},
		{"mongo", func(t *testing.T) Store { return newTestMongoMissionStore(t) }},
	} {
		t.Run(backend.name, func(t *testing.T) {
			t.Run("backlog progresses past unavailable leases", func(t *testing.T) {
				st := backend.open(t)
				ctx := t.Context()
				now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
				until := now.Add(time.Hour)
				// More than a page of both unavailable and eligible work. Insert
				// backwards so equal update times must use the ID tie-break.
				for i := 529; i >= 0; i-- {
					m := reconcileMission(now, fmt.Sprintf("eligible-%03d", i))
					m.CreatedAt = now.Add(-3*time.Hour + time.Duration(i)*time.Second)
					m.State = []State{StateActive, StateWaitingHuman, StateCapabilityBlocked}[i%3]
					if i >= 270 {
						m = reconcileMission(now, fmt.Sprintf("leased-%03d", i))
						m.CreatedAt = now.Add(-3*time.Hour + time.Duration(i)*time.Second)
						m.UpdatedAt = now.Add(-2 * time.Hour)
						m.LeaseOwner, m.LeaseUntil = "other-worker", &until
					}
					createReconcileMission(t, st, m)
				}
				seen := map[string]bool{}
				for page := 0; page < 2; page++ {
					at := now.Add(time.Duration(page) * time.Second)
					rows, err := st.ListReconcileCandidates(ctx, "worker", at, 250)
					if err != nil || len(rows) != 250 {
						t.Fatalf("page %d: count=%d err=%v", page, len(rows), err)
					}
					inPage := map[string]bool{}
					for j, m := range rows {
						if !strings.HasPrefix(m.ID, "eligible-") || inPage[m.ID] {
							t.Fatalf("unavailable or duplicate candidate: %s", m.ID)
						}
						if page == 0 || j < 20 {
							want := fmt.Sprintf("eligible-%03d", page*250+j)
							if m.ID != want {
								t.Fatalf("page %d row %d = %s, want %s", page, j, m.ID, want)
							}
						}
						inPage[m.ID], seen[m.ID] = true, true
						claimed, won, err := st.Claim(ctx, m.ID, "worker", at, 30*time.Second)
						if err != nil || !won {
							t.Fatalf("claim %s: won=%v err=%v", m.ID, won, err)
						}
						if j == 0 {
							// One attempt can persist multiple receipt stages. Each
							// update stays fenced and moves behind older work.
							for stage := 1; stage <= 2; stage++ {
								claimed.UpdatedAt = at.Add(time.Duration(stage) * time.Millisecond)
								claimed, err = st.UpdateClaimed(ctx, claimed, "worker")
								if err != nil {
									t.Fatal(err)
								}
							}
						}
					}
				}
				if len(seen) != 270 {
					t.Fatalf("only %d of 270 eligible missions received an attempt", len(seen))
				}
				rows, err := st.List(ctx, Scope{TenantID: "t", OperatorID: "o"}, 0)
				if err != nil || len(rows) != 530 {
					t.Fatalf("operator history: count=%d err=%v", len(rows), err)
				}
				for i := 1; i < len(rows); i++ {
					if !rows[i-1].CreatedAt.After(rows[i].CreatedAt) {
						t.Fatal("operator history no longer newest-first")
					}
				}
			})
			t.Run("lease boundary and terminal states", func(t *testing.T) {
				st := backend.open(t)
				ctx := t.Context()
				now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
				until := now.Add(30 * time.Second)
				for _, id := range []string{"own", "foreign", "ttl"} {
					m := reconcileMission(now, id)
					if id != "ttl" {
						m.LeaseOwner, m.LeaseUntil = id, &until
					} else {
						m.Policy.ExpiresAt = now.Add(-time.Second)
					}
					createReconcileMission(t, st, m)
				}
				for _, state := range []State{StateCompleted, StateExpired, StateStopped, StateExhausted} {
					m := reconcileMission(now, string(state))
					m.State = state
					createReconcileMission(t, st, m)
				}
				for _, tc := range []struct {
					at   time.Time
					want string
				}{{now, "own,ttl"}, {until.Add(-time.Millisecond), "own,ttl"}, {until, "foreign,own,ttl"}} {
					rows, err := st.ListReconcileCandidates(ctx, "own", tc.at, 0)
					if err != nil {
						t.Fatal(err)
					}
					var ids []string
					for _, m := range rows {
						ids = append(ids, m.ID)
					}
					if got := strings.Join(ids, ","); got != tc.want {
						t.Fatalf("at %s: candidates %s, want %s", tc.at, got, tc.want)
					}
				}
				// A candidate can lose ownership after listing: only Claim is
				// authoritative, even if the earlier snapshot was eligible.
				if _, won, err := st.Claim(ctx, "ttl", "rival", now, time.Minute); err != nil || !won {
					t.Fatalf("rival claim: won=%v err=%v", won, err)
				}
				if _, won, err := st.Claim(ctx, "ttl", "own", now, time.Minute); err != nil || won {
					t.Fatalf("stale candidate bypassed lease: won=%v err=%v", won, err)
				}
			})
		})
	}
}

func reconcileMission(now time.Time, id string) Mission {
	m := testMission(now)
	m.ID, m.InvocationKey, m.TargetRunID = id, "invocation:"+id, "target:"+id
	m.UpdatedAt = now.Add(-time.Hour)
	return m
}

func createReconcileMission(t *testing.T, st Store, m Mission) {
	t.Helper()
	if _, fresh, err := st.CreateOrGet(t.Context(), m); err != nil || !fresh {
		t.Fatalf("create %s: fresh=%v err=%v", m.ID, fresh, err)
	}
}
