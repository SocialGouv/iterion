package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A destination past the journal's read budget saves anyway: the budget is
// the journal's reading bound (#480), not a save's — the destination is
// compared whole, like the single-file door reads its own (#2661). Bounded,
// every save of such a file answered 500 while the same file through the
// single-file door answered 200.
func TestPrepareAuthoringLocal_ReadsADestinationPastTheBudgetWhole(t *testing.T) {
	t.Setenv("ITERION_ASSISTANT_AUTHORING_MAX_TOTAL_BYTES", "64")
	path := filepath.Join(t.TempDir(), "big.txt")
	before := "original\n" + strings.Repeat("x", 200)
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAuthoringLocalLocks(locks) })
	preview := authoringPreviewFile{Scope: "workspace", Path: "big.txt", Operation: "replace", Before: before, After: before + "more\n"}
	tx, err := prepareAuthoringLocal(locks[0], preview, nil, nil)
	if err != nil {
		t.Fatalf("prepare: %v — the destination past the budget must read whole, like the single-file door's", err)
	}
	t.Cleanup(tx.close)
}

// The journal's limit, hit anywhere an authoring write reads, answers 413
// naming the limit and its knob — never a 500 that reads as a server fault.
func TestAuthoringError_Answers413NamingTheKnob(t *testing.T) {
	s, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/teams/t1/files", nil)
	s.authoringError(w, r, errAuthoringReadLimit)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("authoringError = %d: %s, want 413", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ITERION_ASSISTANT_AUTHORING_MAX_TOTAL_BYTES") {
		t.Fatalf("413 does not name the knob: %s", w.Body.String())
	}
}
