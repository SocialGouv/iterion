package mongo

import (
	"context"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newNoteTestStore builds a Mongo store against a throwaway database, or
// skips when ITERION_TEST_MONGO_URI is unset — same gate as the plan
// store test. Run with:
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27017' \
//	    devbox run -- go test ./pkg/store/mongo/ -run Note
func newNoteTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo note-store test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_notes_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	return s
}

// TestRunNoteStore_AppendList locks in the core RunNoteStore contract on
// Mongo: sequential appends get monotonic seqs and list is chronological,
// exactly like the filesystem impl.
func TestRunNoteStore_AppendList(t *testing.T) {
	s := newNoteTestStore(t)
	ctx := store.WithIdentity(context.Background(), "t1", "u1")
	const runID = "run-note-1"

	if _, ok := interface{}(s).(store.RunNoteStore); !ok {
		t.Fatal("mongo store does not satisfy store.RunNoteStore")
	}

	empty, err := s.ListRunNotes(ctx, runID)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no notes, got %d", len(empty))
	}

	a, err := s.AppendRunNote(ctx, runID, store.RunNote{Author: "alice", Body: "flaky, re-ran"})
	if err != nil {
		t.Fatalf("append a: %v", err)
	}
	if a.Seq != 0 {
		t.Errorf("first seq = %d, want 0", a.Seq)
	}
	if a.Timestamp.IsZero() {
		t.Error("timestamp not stamped")
	}

	b, err := s.AppendRunNote(ctx, runID, store.RunNote{Author: "bob", Body: "root cause was X"})
	if err != nil {
		t.Fatalf("append b: %v", err)
	}
	if b.Seq != 1 {
		t.Errorf("second seq = %d, want 1", b.Seq)
	}

	got, err := s.ListRunNotes(ctx, runID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(got))
	}
	if got[0].Body != "flaky, re-ran" || got[1].Body != "root cause was X" {
		t.Errorf("notes out of chronological order: %+v", got)
	}

	// Tenant isolation: a different tenant sees none of t1's notes.
	other := store.WithIdentity(context.Background(), "t2", "u2")
	iso, err := s.ListRunNotes(other, runID)
	if err != nil {
		t.Fatalf("list other tenant: %v", err)
	}
	if len(iso) != 0 {
		t.Errorf("cross-tenant leak: tenant t2 saw %d of t1's notes", len(iso))
	}
}

// TestRunNoteStore_ReattributeRunNotes is the Mongo twin of the filesystem
// test: the address's notes are rewritten in every tenant, from a context
// naming none, however the address was spelled; no other author's note is.
func TestRunNoteStore_ReattributeRunNotes(t *testing.T) {
	s := newNoteTestStore(t)
	t1 := store.WithIdentity(context.Background(), "t1", "u1")
	t2 := store.WithIdentity(context.Background(), "t2", "u2")
	for _, c := range []struct {
		ctx         context.Context
		run, author string
	}{
		{t1, "run-a", "alice@example.org"},
		{t1, "run-a", "bob@example.org"},
		{t2, "run-b", " Alice@Example.org "},
		{t2, "run-b", "alice@example.org.uk"},
	} {
		if _, err := s.AppendRunNote(c.ctx, c.run, store.RunNote{Author: c.author, Body: "note"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	n, err := s.ReattributeRunNotes(context.Background(), "ALICE@example.org", "u-1")
	if err != nil || n != 2 {
		t.Fatalf("ReattributeRunNotes = %d (%v), want 2", n, err)
	}
	for _, c := range []struct {
		ctx  context.Context
		run  string
		want [2]string
	}{
		{t1, "run-a", [2]string{"u-1", "bob@example.org"}},
		{t2, "run-b", [2]string{"u-1", "alice@example.org.uk"}},
	} {
		notes, err := s.ListRunNotes(c.ctx, c.run)
		if err != nil || len(notes) != 2 || notes[0].Author != c.want[0] || notes[1].Author != c.want[1] {
			t.Fatalf("%s notes = %+v (%v), want authors %v", c.run, notes, err, c.want)
		}
	}
}
