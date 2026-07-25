package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// seedRunLog persists raw log bytes for a run through the store's
// RunLogStore — the same interface the cloud runner's runLogWriter
// appends through (Mongo run_logs chunks) and the replay endpoint
// reads back. The filesystem backend stands in here; the Mongo
// backend's identical semantics are pinned by the storetest
// conformance suite (mongo-conformance CI job).
func seedRunLog(t *testing.T, srv *Server, runID string, chunks ...string) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	ls := store.AsRunLogStore(st)
	if ls == nil {
		t.Fatal("filesystem store must implement RunLogStore")
	}
	var off int64
	for i, c := range chunks {
		if err := ls.AppendRunLog(context.Background(), runID, off, []byte(c)); err != nil {
			t.Fatalf("AppendRunLog #%d: %v", i, err)
		}
		off += int64(len(c))
	}
}

// getLog hits the replay endpoint directly (no live buffer exists for
// a seeded terminal run, so this exercises the persisted-store path —
// the one every finished cloud run takes).
func getLog(t *testing.T, srv *Server, runID, query string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID+"/log"+query, nil)
	req.SetPathValue("id", runID)
	rec := httptest.NewRecorder()
	srv.handleGetRunLog(rec, req)
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return rec, string(body)
}

// TestGetRunLogRange proves the replay endpoint serves the persisted
// log for a terminal run — full read, from-offset read, and the
// bounded [from, until) window the studio's replay scrubber uses to
// fetch the head its in-memory buffer evicted.
func TestGetRunLogRange(t *testing.T) {
	srv, _ := newTestServer(t)
	const runID = "run-log-range"
	seedRun(t, srv, runID, "demo", store.RunStatusFinished)
	seedRunLog(t, srv, runID, "hello ", "world")

	t.Run("full", func(t *testing.T) {
		rec, body := getLog(t, srv, runID, "")
		if rec.Code != http.StatusOK || body != "hello world" {
			t.Fatalf("got %d %q; want 200 %q", rec.Code, body, "hello world")
		}
		if got := rec.Header().Get("X-Iterion-Log-Total"); got != "11" {
			t.Fatalf("X-Iterion-Log-Total = %q; want 11", got)
		}
		if got := rec.Header().Get("X-Iterion-Log-Offset"); got != "0" {
			t.Fatalf("X-Iterion-Log-Offset = %q; want 0", got)
		}
	})

	t.Run("from", func(t *testing.T) {
		rec, body := getLog(t, srv, runID, "?from=6")
		if rec.Code != http.StatusOK || body != "world" {
			t.Fatalf("got %d %q; want 200 %q", rec.Code, body, "world")
		}
	})

	t.Run("until window crossing a chunk boundary", func(t *testing.T) {
		rec, body := getLog(t, srv, runID, "?from=4&until=9")
		if rec.Code != http.StatusOK || body != "o wor" {
			t.Fatalf("got %d %q; want 200 %q", rec.Code, body, "o wor")
		}
	})

	t.Run("evicted-head prefix fetch (from=0&until=N)", func(t *testing.T) {
		rec, body := getLog(t, srv, runID, "?from=0&until=6")
		if rec.Code != http.StatusOK || body != "hello " {
			t.Fatalf("got %d %q; want 200 %q", rec.Code, body, "hello ")
		}
	})

	t.Run("negative until reads to end", func(t *testing.T) {
		rec, body := getLog(t, srv, runID, "?until=-3")
		if rec.Code != http.StatusOK || body != "hello world" {
			t.Fatalf("got %d %q; want 200 %q", rec.Code, body, "hello world")
		}
	})
}

// TestGetRunLogEmpty pins the honest-degradation contract: a run with
// no persisted log yields 404, never an empty 200 that would read as a
// captured-but-empty log.
func TestGetRunLogEmpty(t *testing.T) {
	srv, _ := newTestServer(t)
	const runID = "run-log-empty"
	seedRun(t, srv, runID, "demo", store.RunStatusFinished)

	rec, _ := getLog(t, srv, runID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty log: got status %d; want 404", rec.Code)
	}
}

// TestTrimLogWindow covers the live-buffer half of the ?until=
// contract: the snapshot window [snapOffset, snapOffset+len) bounded
// at until.
func TestTrimLogWindow(t *testing.T) {
	data := []byte("abcdef") // covers [10, 16)
	cases := []struct {
		name  string
		until int64
		want  []byte
	}{
		{"unbounded", 0, data},
		{"negative means unbounded", -1, data},
		{"entirely before window", 10, nil},
		{"mid-window", 13, []byte("abc")},
		{"exactly window end", 16, data},
		{"past window end", 99, data},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trimLogWindow(10, data, c.until); !bytes.Equal(got, c.want) {
				t.Fatalf("trimLogWindow(10, %q, %d) = %q; want %q", data, c.until, got, c.want)
			}
		})
	}
}
