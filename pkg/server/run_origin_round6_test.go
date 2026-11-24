package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// F1: a default studio configures no StoreDir — its store is resolved from
// the work dir — and still knows that store as its own: a `?store=` naming it
// with the current birth is the primary run's request, served, and the
// global-active row of its run says own_store.
func TestRunOriginGateDefaultStudioKnowsItsOwnStore(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	srv := New(Config{WorkDir: t.TempDir(), SkipProjectRegistration: true, DisableAuth: true}, iterlog.New(iterlog.LevelError, os.Stderr))
	if srv.runs == nil || srv.cfg.StoreDir != "" {
		t.Fatalf("fixture: run console %v, configured StoreDir %q", srv.runs != nil, srv.cfg.StoreDir)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hs.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	root := srv.runs.StoreDir()
	native, err := store.New(root)
	mustWS(t, err)
	mustWS(t, seedWSOriginRun(t.Context(), native, "r", "OWN", store.LegacyValueFormat, store.RunStatusRunning, 0))
	birth := birthOf(t, native, "r")

	got := gateDo(t, hs.URL, http.MethodGet, "/api/runs/r?store="+url.QueryEscape(root), birth, "header")
	if got.status != http.StatusOK || !strings.Contains(got.body, `"workflow_name":"OWN"`) {
		t.Fatalf("?store=<the resolved store> with the current birth: %d %.200s, want the run", got.status, got.body)
	}

	resp, err := http.Get(hs.URL + "/api/runs/global-active")
	mustWS(t, err)
	defer resp.Body.Close()
	var listed struct {
		Runs []globalActiveRun `json:"runs"`
	}
	mustWS(t, json.NewDecoder(resp.Body).Decode(&listed))
	for _, row := range listed.Runs {
		if row.ID == "r" {
			if !row.OwnStore {
				t.Fatalf("the global-active row of the studio's own run: own_store false (store_path %s, resolved %s)", row.StorePath, root)
			}
			return
		}
	}
	t.Fatalf("the studio's own running run is not listed: %+v", listed.Runs)
}

// F7: ITERION_RUN_BIRTH_CHECK=0 lifts the birth comparison, not the scope of
// the guarantee: an expectation for a foreign store's run is still refused
// (400 store_unsupported, a socket closed with that reason), while one for
// the server's own run is admitted unread.
func TestRunOriginGateForeignStoreRefusedWithTheCheckOff(t *testing.T) {
	t.Setenv(runBirthCheckEnv, "0")
	fx := newGateFixture(t, true)
	xdir, foreign, _ := crossStoreFixture(t, fx)
	q := "?store=" + url.QueryEscape(xdir)
	for _, how := range []string{"header", "query"} {
		if got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r"+q, foreign, how); got.status != http.StatusBadRequest || got.code != errCodeStoreUnsupported {
			t.Fatalf("foreign store, birth in the %s, check off: %d %.160s, want 400 %s", how, got.status, got.body, errCodeStoreUnsupported)
		}
	}
	c := dialGate(t, fx.hs, "/api/ws/runs/r"+q+"&expected_birth="+foreign)
	if ce, texts := awaitWSClose(t, c, 10*time.Second); len(texts) != 0 || !strings.HasPrefix(ce.Text, errCodeStoreUnsupported+": ") {
		t.Fatalf("the run socket, check off: frames %v close %q, want the %s close alone", texts, ce.Text, errCodeStoreUnsupported)
	}
	if got := gateDo(t, fx.hs.URL, http.MethodGet, "/api/runs/r", staleBirth, "header"); got.status != http.StatusOK {
		t.Fatalf("the server's own run with a stale birth, check off: %d %.160s, want it admitted unread", got.status, got.body)
	}
}
