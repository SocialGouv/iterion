package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestNotes_Endpoint pins the /api/runs/:id/notes contract: POST persists
// a note and returns it (201); GET lists notes chronologically (200, []
// when none); the round-trip survives a fresh store handle (persisted,
// not in-memory); bad input and unknown runs are rejected.
func TestNotes_Endpoint(t *testing.T) {
	srv, hs := newTestServer(t)
	const runID = "note-run"
	seedRun(t, srv, runID, "wf", store.RunStatusFinished)

	t.Run("empty when no notes added", func(t *testing.T) {
		notes := getNotes(t, hs.URL, runID)
		if len(notes) != 0 {
			t.Fatalf("expected empty notes, got %d", len(notes))
		}
	})

	t.Run("POST adds a note and returns it (201)", func(t *testing.T) {
		note := addNote(t, hs.URL, runID, `{"body":"flaky, re-ran","author":"alice"}`, http.StatusCreated)
		if note.Seq != 0 {
			t.Errorf("first note seq = %d, want 0", note.Seq)
		}
		if note.Body != "flaky, re-ran" || note.Author != "alice" {
			t.Errorf("unexpected note: %+v", note)
		}
		if note.Timestamp.IsZero() {
			t.Error("timestamp not stamped")
		}
	})

	t.Run("author defaults to operator when omitted", func(t *testing.T) {
		note := addNote(t, hs.URL, runID, `{"body":"root cause was X"}`, http.StatusCreated)
		if note.Author != "operator" {
			t.Errorf("author = %q, want operator", note.Author)
		}
		if note.Seq != 1 {
			t.Errorf("second note seq = %d, want 1", note.Seq)
		}
	})

	t.Run("GET lists notes chronologically", func(t *testing.T) {
		notes := getNotes(t, hs.URL, runID)
		if len(notes) != 2 {
			t.Fatalf("expected 2 notes, got %d", len(notes))
		}
		if notes[0].Body != "flaky, re-ran" || notes[1].Body != "root cause was X" {
			t.Errorf("notes out of order: %+v", notes)
		}
	})

	t.Run("notes persist across a fresh store handle", func(t *testing.T) {
		st, err := store.New(srv.cfg.StoreDir)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		got, err := st.ListRunNotes(nil, runID) //nolint:staticcheck // fs impl ignores ctx
		if err != nil {
			t.Fatalf("ListRunNotes: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 persisted notes, got %d", len(got))
		}
	})

	t.Run("empty body is 400", func(t *testing.T) {
		addNote(t, hs.URL, runID, `{"body":"   "}`, http.StatusBadRequest)
	})

	t.Run("unknown run is 404 on GET", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs/does-not-exist/notes")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("unknown run is 404 on POST", func(t *testing.T) {
		addNoteToRun(t, hs.URL, "does-not-exist", `{"body":"x"}`, http.StatusNotFound)
	})
}

func getNotes(t *testing.T, base, runID string) []store.RunNote {
	t.Helper()
	resp, err := http.Get(base + "/api/runs/" + runID + "/notes")
	if err != nil {
		t.Fatalf("GET notes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Notes []store.RunNote `json:"notes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Notes
}

func addNote(t *testing.T, base, runID, body string, wantStatus int) store.RunNote {
	t.Helper()
	return addNoteToRun(t, base, runID, body, wantStatus)
}

func addNoteToRun(t *testing.T, base, runID, body string, wantStatus int) store.RunNote {
	t.Helper()
	resp, err := http.Post(base+"/api/runs/"+runID+"/notes", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST note: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d", resp.StatusCode, wantStatus)
	}
	var note store.RunNote
	if resp.StatusCode == http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&note); err != nil {
			t.Fatalf("decode note: %v", err)
		}
	}
	return note
}
