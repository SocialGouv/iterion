package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/cli"
)

// The PUT replaces the WHOLE policy, so the document this CLI sends carries
// every other admin's reservations as it last read them. When the server
// refuses a stale write (409), replaying the operator's own edit onto a fresh
// read is what keeps the other admin's reservation alive — writing the stale
// document again, or giving up, both lose it.
func TestBudgetFloorEdit_RetriesOnceOnAStaleWriteAndKeepsTheOtherAdminsEntry(t *testing.T) {
	var mu sync.Mutex
	// The server's state, as a second admin leaves it: they added feature-dev
	// AFTER this CLI read the policy.
	stored := budgetfloor.Policy{
		Reservations: []budgetfloor.Reservation{
			{BotID: "feature-dev", Reserve: budgetfloor.Reserve{FiveHourPercent: 10}},
		},
		UpdatedAt: time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC),
	}
	var puts []budgetfloor.Policy
	gets := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			gets++
			_ = json.NewEncoder(w).Encode(map[string]any{"stored": stored})
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var got budgetfloor.Policy
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("undecodable PUT body: %v", err)
			}
			puts = append(puts, got)
			// The CAS: only a write carrying the token we last stamped wins.
			if !got.UpdatedAt.Equal(stored.UpdatedAt) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte("the budget floor changed since it was read"))
				return
			}
			stored = got
			stored.UpdatedAt = stored.UpdatedAt.Add(time.Minute)
			_ = json.NewEncoder(w).Encode(map[string]any{"stored": stored})
		}
	}))
	defer srv.Close()

	// First read is stale by one edit; the retry reads the current one.
	staleToken := stored.UpdatedAt.Add(-time.Hour)
	firstRead := budgetfloor.Policy{UpdatedAt: staleToken}

	c := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: srv.URL})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	p := &cli.Printer{W: &bytes.Buffer{}}

	firstFetch := true
	edit := func(pol budgetfloor.Policy) (budgetfloor.Policy, error) {
		if firstFetch {
			// Model the read that happened before the other admin's write.
			pol, firstFetch = firstRead, false
		}
		pol.Reservations = upsertReservation(pol.Reservations, budgetfloor.Reservation{
			BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 20},
		})
		return pol, nil
	}
	if err := editFloorPolicy(cmd, c, p, edit); err != nil {
		t.Fatalf("edit: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(puts) != 2 || gets != 2 {
		t.Fatalf("%d GET(s) / %d PUT(s), want 2 and 2 — a 409 must be answered by re-reading, not by giving up", gets, len(puts))
	}
	if _, ok := stored.Reserved("review-pr"); !ok {
		t.Error("the operator's own edit did not land")
	}
	if _, ok := stored.Reserved("feature-dev"); !ok {
		t.Error("the concurrent admin's reservation was overwritten by the replay")
	}
}

// A 409 that keeps coming back is the operator's business: a loop that never
// gives up would hide a genuinely contended policy behind an infinite retry.
func TestBudgetFloorEdit_GivesUpAfterOneRetry(t *testing.T) {
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"stored":{}}`))
			return
		}
		puts++
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("changed concurrently"))
	}))
	defer srv.Close()

	c := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: srv.URL})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	p := &cli.Printer{W: &bytes.Buffer{}}
	err := editFloorPolicy(cmd, c, p, func(pol budgetfloor.Policy) (budgetfloor.Policy, error) {
		pol.Reservations = upsertReservation(pol.Reservations, budgetfloor.Reservation{
			BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 20},
		})
		return pol, nil
	})
	if err == nil {
		t.Fatal("a policy that stays contended must surface the 409")
	}
	if puts != 2 {
		t.Fatalf("%d PUT(s), want exactly 2 (one attempt, one retry)", puts)
	}
}

// `reserve` and `quota` EDIT one entry; an axis the operator did not name is
// left as stored. Replacing the whole entry from flag defaults instead makes
// the natural second command — adding a slot reserve to a bot that already has
// a window band — silently zero that band: the default axis, the only one that
// measures what actually runs out on a subscription, gone with nothing to
// distinguish it from a deliberate clear.
func TestApplyFloorEdit_EditsOneAxisAndLeavesTheRest(t *testing.T) {
	stored := budgetfloor.Policy{
		Reservations: []budgetfloor.Reservation{
			{BotID: "review-pr", Note: "keeps review moving", Reserve: budgetfloor.Reserve{FiveHourPercent: 20}},
		},
		RepoQuotas: []budgetfloor.RepoQuota{{Repo: "o/r", MonthlyUSD: 50}},
	}

	t.Run("a new axis joins the stored ones", func(t *testing.T) {
		remoteFloorBot, remoteFloorSlots = "review-pr", 2
		t.Cleanup(func() { remoteFloorBot, remoteFloorSlots = "", 0 })
		got, err := applyFloorEdit(stored, floorEdit{action: "reserve", named: map[string]bool{"bot": true, "concurrent-runs": true}})
		if err != nil {
			t.Fatalf("edit: %v", err)
		}
		res, ok := got.Reserved("review-pr")
		if !ok {
			t.Fatal("the reservation vanished")
		}
		if res.Reserve.FiveHourPercent != 20 {
			t.Errorf("five_hour = %d, want the stored 20 — an unnamed axis must not be zeroed", res.Reserve.FiveHourPercent)
		}
		if res.Reserve.ConcurrentRuns != 2 {
			t.Errorf("concurrent_runs = %d, want 2", res.Reserve.ConcurrentRuns)
		}
		if res.Note != "keeps review moving" {
			t.Errorf("note = %q, want the stored one", res.Note)
		}
	})

	t.Run("naming an axis with 0 clears it", func(t *testing.T) {
		remoteFloorBot, remoteFloorFiveHour, remoteFloorSlots = "review-pr", 0, 2
		t.Cleanup(func() { remoteFloorBot, remoteFloorFiveHour, remoteFloorSlots = "", 0, 0 })
		got, err := applyFloorEdit(stored, floorEdit{action: "reserve",
			named: map[string]bool{"bot": true, "five-hour": true, "concurrent-runs": true}})
		if err != nil {
			t.Fatalf("edit: %v", err)
		}
		res, _ := got.Reserved("review-pr")
		if res.Reserve.FiveHourPercent != 0 {
			t.Errorf("five_hour = %d, want it cleared — naming a flag is how an axis is set to zero", res.Reserve.FiveHourPercent)
		}
	})

	t.Run("a quota keeps the axes it does not name", func(t *testing.T) {
		remoteFloorRepo, remoteFloorRepoSpends = "o/r", 40
		t.Cleanup(func() { remoteFloorRepo, remoteFloorRepoSpends = "", 0 })
		got, err := applyFloorEdit(stored, floorEdit{action: "quota",
			named: map[string]bool{"repo": true, "route-spends-per-month": true}})
		if err != nil {
			t.Fatalf("edit: %v", err)
		}
		usd, spends := got.RepoCap("o/r")
		if usd != 50 || spends != 40 {
			t.Errorf("quota = $%.2f / %d, want $50 / 40 — the stored amount must survive", usd, spends)
		}
	})

	t.Run("rm removes an entry stored with stray whitespace", func(t *testing.T) {
		padded := budgetfloor.Policy{
			Reservations: []budgetfloor.Reservation{{BotID: " review-pr ", Reserve: budgetfloor.Reserve{FiveHourPercent: 20}}},
			RepoQuotas:   []budgetfloor.RepoQuota{{Repo: " o/r ", MonthlyUSD: 50}},
		}
		remoteFloorBot, remoteFloorRepo = "review-pr", "o/r"
		t.Cleanup(func() { remoteFloorBot, remoteFloorRepo = "", "" })
		got, err := applyFloorEdit(padded, floorEdit{action: "rm", named: map[string]bool{"bot": true, "repo": true}})
		if err != nil {
			t.Fatalf("rm: %v", err)
		}
		if len(got.Reservations) != 0 || len(got.RepoQuotas) != 0 {
			t.Fatalf("rm left %+v — an id the API accepted padded must still be removable", got)
		}
	})
}

// applyFloorEdit's doc calls it a pure function of the policy it is applied
// to, and the CAS retry is what makes that load-bearing: on a 409 the SAME
// edit is replayed onto a freshly read document, and it is only the same edit
// if applying it left nothing behind in the first one.
//
// The helpers used to filter and overwrite in place (`in[:0]`, `in[i] = res`),
// which rewrites the CALLER's slice through the shared backing array. Nothing
// broke only because every retry re-fetches; a caller that holds a policy
// across the call would have found its reservations quietly replaced.
func TestApplyFloorEdit_DoesNotMutateThePolicyItIsGiven(t *testing.T) {
	edits := []struct {
		name string
		bot  string
		repo string
		e    floorEdit
	}{
		{"rm a reservation", "review-pr", "", floorEdit{action: "rm", named: map[string]bool{"bot": true}}},
		{"rm a repo quota", "", "o/a", floorEdit{action: "rm", named: map[string]bool{"repo": true}}},
		{"overwrite a reservation axis", "review-pr", "", floorEdit{action: "reserve", named: map[string]bool{"bot": true, "five-hour": true}}},
		{"overwrite a repo quota axis", "", "o/a", floorEdit{action: "quota", named: map[string]bool{"repo": true, "monthly-usd": true}}},
	}
	for _, tc := range edits {
		t.Run(tc.name, func(t *testing.T) {
			stored := budgetfloor.Policy{
				Reservations: []budgetfloor.Reservation{
					{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 20}},
					{BotID: "feature-dev", Reserve: budgetfloor.Reserve{FiveHourPercent: 10}},
				},
				RepoQuotas: []budgetfloor.RepoQuota{
					{Repo: "o/a", MonthlyUSD: 10},
					{Repo: "o/b", MonthlyUSD: 20},
				},
			}
			// The comparison is against what the caller HAD, captured by value
			// before the call — an entry that moved or vanished in `stored`
			// itself is the corruption, whatever the return value looks like.
			wantRes := append([]budgetfloor.Reservation(nil), stored.Reservations...)
			wantQuotas := append([]budgetfloor.RepoQuota(nil), stored.RepoQuotas...)

			remoteFloorBot, remoteFloorRepo = tc.bot, tc.repo
			remoteFloorFiveHour, remoteFloorUSD = 40, 99
			t.Cleanup(func() {
				remoteFloorBot, remoteFloorRepo = "", ""
				remoteFloorFiveHour, remoteFloorUSD = 0, 0
			})
			if _, err := applyFloorEdit(stored, tc.e); err != nil {
				t.Fatalf("applyFloorEdit: %v", err)
			}

			if len(stored.Reservations) != len(wantRes) || len(stored.RepoQuotas) != len(wantQuotas) {
				t.Fatalf("the input policy changed shape: %d/%d reservations/quotas, want %d/%d",
					len(stored.Reservations), len(stored.RepoQuotas), len(wantRes), len(wantQuotas))
			}
			for i := range wantRes {
				if stored.Reservations[i] != wantRes[i] {
					t.Errorf("input reservation %d became %+v, want the untouched %+v", i, stored.Reservations[i], wantRes[i])
				}
			}
			for i := range wantQuotas {
				if stored.RepoQuotas[i] != wantQuotas[i] {
					t.Errorf("input quota %d became %+v, want the untouched %+v", i, stored.RepoQuotas[i], wantQuotas[i])
				}
			}
		})
	}
}
