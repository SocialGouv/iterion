package boardmongo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

func rva19db(t *testing.T, prefix string) (*mongo.Database, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database(prefix + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

// A CONCURRENT writer removes the very label the sweep wanted to delete,
// between the sweep's read and its write. The re-read + re-transform then
// has nothing to do — the DESIRED end state already holds. The sweep must
// converge, not report "lost the label CAS" and abort the remaining cards.
func TestLabelSweep_ConcurrentRemovalIsNotAnExhaustion(t *testing.T) {
	db, ctx := rva19db(t, "rva19a_")
	s := New(db, "t1")
	first, err := s.Create(native.Issue{Title: "a", State: native.StateReady, Labels: []string{"retire-me"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(native.Issue{Title: "b", State: native.StateReady, Labels: []string{"retire-me"}})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	del := func(labels []string) ([]string, bool) {
		n++
		if n == 1 {
			// Someone else removes the label first (an operator, another
			// replica, the trigger spine's consume) — with a CAS-missing
			// write so the sweep's guard misses.
			fresh := []string{"unrelated"}
			if _, uerr := s.Update(first.ID, native.Patch{Labels: &fresh}); uerr != nil {
				t.Fatal(uerr)
			}
		}
		out := make([]string, 0, len(labels))
		changed := false
		for _, l := range labels {
			if l == "retire-me" {
				changed = true
				continue
			}
			out = append(out, l)
		}
		return out, changed
	}
	touched, err := s.applyLabelRewrite(ctx, del, native.EvtIssueUpdated, map[string]any{"op": "delete"})
	if err != nil {
		t.Fatalf("converged state reported as a failure: touched=%d err=%v", touched, err)
	}
	got, err := s.Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range got.Labels {
		if l == "retire-me" {
			t.Fatalf("the sweep aborted before the SECOND card: labels=%v — the operator's delete silently did not land there", got.Labels)
		}
	}
}

// A card DELETED mid-sweep is a benign race the code says it tolerates
// ("deleted mid-sweep — a benign race, like the FS twin"). It must not be
// reported as a CAS exhaustion, and must not abort the walk.
func TestLabelSweep_DeletedMidSweepIsBenign(t *testing.T) {
	db, ctx := rva19db(t, "rva19b_")
	s := New(db, "t1")
	victim, err := s.Create(native.Issue{Title: "a", State: native.StateReady, Labels: []string{"retire-me"}})
	if err != nil {
		t.Fatal(err)
	}
	survivor, err := s.Create(native.Issue{Title: "b", State: native.StateReady, Labels: []string{"retire-me"}})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	del := func(labels []string) ([]string, bool) {
		n++
		if n == 1 {
			if derr := s.Delete(victim.ID); derr != nil {
				t.Fatal(derr)
			}
		}
		out := make([]string, 0, len(labels))
		changed := false
		for _, l := range labels {
			if l == "retire-me" {
				changed = true
				continue
			}
			out = append(out, l)
		}
		return out, changed
	}
	if _, err := s.applyLabelRewrite(ctx, del, native.EvtIssueUpdated, map[string]any{"op": "delete"}); err != nil {
		t.Fatalf("a card deleted mid-sweep aborted the whole rewrite: %v", err)
	}
	got, err := s.Get(survivor.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range got.Labels {
		if l == "retire-me" {
			t.Fatalf("the sweep aborted before the survivor: labels=%v", got.Labels)
		}
	}
}

// One PERPETUALLY contended card aborts the walk on every retry, so every
// card ordered after it keeps the label for ever — no number of operator
// re-runs can drain the board.
func TestLabelSweep_OneHotCardDoesNotBlockTheWalk(t *testing.T) {
	db, ctx := rva19db(t, "rva19c_")
	s := New(db, "t1")
	var ids []string
	for i := 0; i < 4; i++ {
		iss, err := s.Create(native.Issue{Title: "c", State: native.StateReady, Labels: []string{"retire-me"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, iss.ID)
	}
	all, err := s.listAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hot := all[0].ID // whichever card the walk reaches first
	n := 0
	del := func(labels []string) ([]string, bool) {
		n++
		out := make([]string, 0, len(labels))
		changed := false
		for _, l := range labels {
			if l == "retire-me" {
				changed = true
				continue
			}
			out = append(out, l)
		}
		return out, changed
	}
	// A writer that keeps the hot card moving under the sweep.
	sabotage := func(labels []string) ([]string, bool) {
		fresh := []string{"retire-me", fmt.Sprintf("noise-%d", n)}
		if _, uerr := s.Update(hot, native.Patch{Labels: &fresh}); uerr != nil {
			t.Fatal(uerr)
		}
		return del(labels)
	}
	for round := 0; round < 3; round++ {
		if _, err := s.applyLabelRewrite(ctx, sabotage, native.EvtIssueUpdated, map[string]any{"op": "delete"}); err == nil {
			t.Fatalf("round %d unexpectedly succeeded", round)
		}
	}
	stuck := 0
	for _, id := range ids {
		if id == hot {
			continue
		}
		got, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range got.Labels {
			if l == "retire-me" {
				stuck++
			}
		}
	}
	if stuck > 0 {
		t.Fatalf("%d/%d cold cards STILL carry the label after 3 operator re-runs — one hot card aborts the walk every time", stuck, len(ids)-1)
	}
}
