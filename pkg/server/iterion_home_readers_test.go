package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/store"
)

// decoyDotIterion points $HOME at a fresh directory holding an .iterion the
// readers must ignore once ITERION_HOME names the iterion home.
func decoyDotIterion(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	decoy := filepath.Join(home, ".iterion")
	if err := os.MkdirAll(filepath.Join(decoy, "projects", "-decoy"), 0o700); err != nil {
		t.Fatal(err)
	}
	return decoy
}

// The global runs view scans the stores under the iterion home the writers
// use ($ITERION_HOME), not a $HOME/.iterion no store was written to.
func TestGlobalStoreRoots_ScanTheIterionHome(t *testing.T) {
	iterionHome := t.TempDir()
	t.Setenv("ITERION_HOME", iterionHome)
	decoyDotIterion(t)
	for _, key := range []string{"-a", "-b"} {
		if err := os.MkdirAll(filepath.Join(iterionHome, "projects", key), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	roots, err := globalStoreRoots()
	if err != nil {
		t.Fatalf("globalStoreRoots: %v", err)
	}
	want := []string{iterionHome, filepath.Join(iterionHome, "projects", "-a"), filepath.Join(iterionHome, "projects", "-b")}
	if !reflect.DeepEqual(roots, want) {
		t.Fatalf("globalStoreRoots = %v, want the iterion home and its projects %v", roots, want)
	}
}

// The cross-store proxy accepts a store under the iterion home and refuses a
// $HOME/.iterion that is not it.
func TestResolveCrossStore_TheRootIsTheIterionHome(t *testing.T) {
	srv, _ := newTestServer(t)
	iterionHome := t.TempDir()
	t.Setenv("ITERION_HOME", iterionHome)
	decoy := decoyDotIterion(t)

	resolve := func(path string) (string, error) {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/x?store="+url.QueryEscape(path), nil)
		_, root, err := srv.resolveCrossStore(req)
		return root, err
	}

	root, err := resolve(iterionHome)
	if err != nil {
		t.Fatalf("a store under the iterion home was refused: %v", err)
	}
	if want, _ := filepath.EvalSymlinks(iterionHome); root != want {
		t.Fatalf("resolved store = %q, want %q", root, want)
	}
	if _, err := resolve(decoy); err == nil || !strings.Contains(err.Error(), "outside the iterion home") {
		t.Fatalf("$HOME/.iterion outside ITERION_HOME: err = %v, want a refusal", err)
	}
}

// A home a project `.env` planted is not the operator's: the cross-store proxy
// refuses a store under it — a planted ITERION_HOME=/ would otherwise open any
// path on the host — and the global view does not list it. Both stay on the
// home the operator chose, resolved as a production binary does.
func TestTheReadersIgnoreAPlantedITERION_HOME(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	srv, _ := newTestServer(t)
	operator := t.TempDir()
	t.Setenv("HOME", operator)
	chosen := filepath.Join(operator, ".iterion")
	planted := t.TempDir()
	for _, dir := range []string{filepath.Join(chosen, "projects", "-a"), filepath.Join(planted, "projects", "-p")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ITERION_HOME", planted)
	envtrust.MarkPlanted("ITERION_HOME")
	store.ResolveIterionHomeAsInProductionForTests(t)

	resolve := func(path string) (string, error) {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/x?store="+url.QueryEscape(path), nil)
		_, root, err := srv.resolveCrossStore(req)
		return root, err
	}
	if root, err := resolve(planted); err == nil || !strings.Contains(err.Error(), "outside the iterion home") {
		t.Fatalf("a store under the planted ITERION_HOME was served (root %q, err %v); want a refusal", root, err)
	}
	if _, err := resolve(chosen); err != nil {
		t.Fatalf("a store under the operator's iterion home was refused: %v", err)
	}
	roots, err := globalStoreRoots()
	if err != nil {
		t.Fatalf("globalStoreRoots: %v", err)
	}
	if want := []string{chosen, filepath.Join(chosen, "projects", "-a")}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("globalStoreRoots = %v, want the operator's iterion home %v", roots, want)
	}
}

// A home dir a project `.env` planted — the operator exported neither HOME nor
// ITERION_HOME — names no home for the readers either: the proxy refuses a
// store under it with the no-home refusal and the global view lists nothing,
// rather than fall back to the live value the `.env` chose.
func TestTheReadersIgnoreAPlantedHOME(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	srv, _ := newTestServer(t)
	planted := t.TempDir()
	under := filepath.Join(planted, ".iterion", "projects", "-p")
	if err := os.MkdirAll(under, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_HOME", "")
	t.Setenv("HOME", planted)
	envtrust.MarkPlanted("HOME")
	store.ResolveIterionHomeAsInProductionForTests(t)

	req := httptest.NewRequest(http.MethodGet, "/api/runs/x?store="+url.QueryEscape(under), nil)
	if _, root, err := srv.resolveCrossStore(req); err == nil || !strings.Contains(err.Error(), "resolve the iterion home") {
		t.Fatalf("a store under a planted HOME: root %q, err %v; want the no-home refusal", root, err)
	}
	if roots, err := globalStoreRoots(); err != nil || roots != nil {
		t.Fatalf("globalStoreRoots under a planted HOME = %v, %v; want nothing", roots, err)
	}
}

// The global active-runs view lists a paused run of a per-project store under
// the iterion home — not the decoy's — and a cloud instance, whose home is
// shared infrastructure, lists nothing.
func TestGlobalActiveRuns_ListTheIterionHomeLocallyAndNothingOnCloud(t *testing.T) {
	srv, _ := newTestServer(t)
	iterionHome := t.TempDir()
	t.Setenv("ITERION_HOME", iterionHome)
	decoy := decoyDotIterion(t)
	ctx := context.Background()
	for _, spot := range []struct{ dir, id string }{
		{filepath.Join(iterionHome, "projects", "-p"), "r-home"},
		{filepath.Join(decoy, "projects", "-decoy"), "r-decoy"},
	} {
		st, err := store.New(spot.dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateRun(ctx, spot.id, "wf", nil); err != nil {
			t.Fatal(err)
		}
		if err := st.PauseRun(ctx, spot.id, &store.Checkpoint{NodeID: "gate", InteractionID: "I1"}); err != nil {
			t.Fatal(err)
		}
	}

	list := func() []globalActiveRun {
		rec := httptest.NewRecorder()
		srv.handleListGlobalActiveRuns(rec, httptest.NewRequest(http.MethodGet, "/api/runs/global-active", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("global-active: status %d (body %s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Runs []globalActiveRun `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Runs
	}

	runs := list()
	if len(runs) != 1 || runs[0].ID != "r-home" || runs[0].StorePath != filepath.Join(iterionHome, "projects", "-p") {
		t.Fatalf("local global-active = %+v, want only r-home from %s", runs, filepath.Join(iterionHome, "projects", "-p"))
	}
	srv.cfg.Mode = "cloud"
	if runs := list(); len(runs) != 0 {
		t.Fatalf("cloud global-active = %+v, want nothing", runs)
	}
}

// The containment check's live branches: a per-project store under the home
// is served, a sibling sharing the home's name as a prefix is refused, and a
// home reached through a symlink serves its stores by either path.
func TestResolveCrossStore_ContainmentBranches(t *testing.T) {
	srv, _ := newTestServer(t)
	resolve := func(path string) (string, error) {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/x?store="+url.QueryEscape(path), nil)
		_, root, err := srv.resolveCrossStore(req)
		return root, err
	}
	mkdir := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}

	target := t.TempDir()
	nested := mkdir(filepath.Join(target, "projects", "-k"))
	sibling := mkdir(target + "2")
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	wantNested, err := filepath.EvalSymlinks(nested)
	if err != nil {
		t.Fatal(err)
	}

	for _, home := range []string{target, link} {
		t.Setenv("ITERION_HOME", home)
		for _, path := range []string{nested, filepath.Join(link, "projects", "-k")} {
			if root, err := resolve(path); err != nil || root != wantNested {
				t.Fatalf("ITERION_HOME=%s: a store at %s = %q, %v; want it served as %s", home, path, root, err, wantNested)
			}
		}
		if _, err := resolve(sibling); err == nil || !strings.Contains(err.Error(), "outside the iterion home") {
			t.Fatalf("ITERION_HOME=%s: the prefix sibling %s: err = %v, want a refusal", home, sibling, err)
		}
	}
}

// Without a resolvable home dir the readers trust nothing: the writers fall
// back to the shared <tmp>/iterion-data, which anyone on the host can
// pre-create — the global view lists none of it and the proxy refuses it.
func TestReaders_WithoutAHomeTrustNothingOfTheSharedFallback(t *testing.T) {
	srv, _ := newTestServer(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", "")
	t.Setenv("ITERION_HOME", "")
	store.ResolveIterionHomeAsInProductionForTests(t)
	fallback := store.GlobalIterionDataDir()
	if fallback != filepath.Join(tmp, "iterion-data") {
		t.Fatalf("the writers' fallback is %q, want %s — the scenario no longer exercises the shared fallback", fallback, filepath.Join(tmp, "iterion-data"))
	}
	planted := filepath.Join(fallback, "projects", "-planted")
	st, err := store.New(planted)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "r-planted", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.PauseRun(ctx, "r-planted", &store.Checkpoint{NodeID: "gate", InteractionID: "I1"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.handleListGlobalActiveRuns(rec, httptest.NewRequest(http.MethodGet, "/api/runs/global-active", nil))
	var out struct {
		Runs []globalActiveRun `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("global-active: status %d, decode err %v (body %s)", rec.Code, err, rec.Body.String())
	}
	if len(out.Runs) != 0 {
		t.Fatalf("a home-less global view listed the shared fallback: %+v", out.Runs)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/runs/r-planted?store="+url.QueryEscape(planted), nil)
	if _, root, err := srv.resolveCrossStore(req); err == nil {
		t.Fatalf("a home-less proxy served the shared fallback store %s", root)
	}
}
