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
