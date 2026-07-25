package server

import (
	"net/http"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// seedShellRun writes a run with the given status + workdir.
func seedShellRun(t *testing.T, srv *Server, runID string, status store.RunStatus, workDir string) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = status
	r.WorkDir = workDir
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
}

// TestRunShell_Gates exercises every pre-upgrade refusal: the gate
// answers plain HTTP (no broken WS handshake) with the truthful code.
func TestRunShell_Gates(t *testing.T) {
	srv, hs := newTestServer(t)
	wt := t.TempDir()
	seedShellRun(t, srv, "r-live", store.RunStatusRunning, wt)
	seedShellRun(t, srv, "r-no-wd", store.RunStatusFailed, "")
	seedShellRun(t, srv, "r-gone", store.RunStatusFailed, wt+"/absent")
	seedShellRun(t, srv, "r-ok", store.RunStatusFailed, wt)

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(hs.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	t.Run("unknown run 404", func(t *testing.T) {
		resp := get("/api/ws/runs/absent/shell")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("live run 409", func(t *testing.T) {
		resp := get("/api/ws/runs/r-live/shell")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})
	t.Run("no workdir 409", func(t *testing.T) {
		resp := get("/api/ws/runs/r-no-wd/shell")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})
	t.Run("worktree gone 410", func(t *testing.T) {
		resp := get("/api/ws/runs/r-gone/shell")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusGone {
			t.Fatalf("status = %d, want 410", resp.StatusCode)
		}
	})
	t.Run("cross-store 409", func(t *testing.T) {
		resp := get("/api/ws/runs/r-ok/shell?store=/elsewhere")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})
	t.Run("eligible run reaches the upgrade", func(t *testing.T) {
		// A plain GET (no Upgrade headers) that passed every gate fails
		// AT the websocket handshake — anything but the gate codes.
		resp := get("/api/ws/runs/r-ok/shell")
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusConflict, http.StatusGone, http.StatusForbidden:
			t.Fatalf("status = %d — a gate rejected an eligible run", resp.StatusCode)
		}
	})
}

func TestShellEligible(t *testing.T) {
	for status, want := range map[store.RunStatus]bool{
		store.RunStatusRunning:            false,
		store.RunStatusQueued:             false,
		store.RunStatusFinished:           true,
		store.RunStatusFailed:             true,
		store.RunStatusFailedResumable:    true,
		store.RunStatusCancelled:          true,
		store.RunStatusPausedWaitingHuman: true,
		store.RunStatusPausedOperator:     true,
	} {
		if got := shellEligible(status); got != want {
			t.Errorf("shellEligible(%s) = %v, want %v", status, got, want)
		}
	}
}

func TestShellTimeoutsEnvOverride(t *testing.T) {
	if err := os.Setenv("ITERION_RUN_SHELL_IDLE_TIMEOUT", "5m"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("ITERION_RUN_SHELL_IDLE_TIMEOUT")
	if got := shellIdleTimeout().Minutes(); got != 5 {
		t.Fatalf("idle timeout = %vm, want 5m", got)
	}
	// Garbage value falls back to the default, never zero.
	_ = os.Setenv("ITERION_RUN_SHELL_IDLE_TIMEOUT", "bogus")
	if got := shellIdleTimeout(); got != defaultShellIdleTimeout {
		t.Fatalf("idle timeout = %v, want default on bogus env", got)
	}
}
