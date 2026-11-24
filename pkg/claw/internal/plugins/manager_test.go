package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

// fixtureManager wires a manager against an httptest tarball + catalog
// so install/uninstall tests run end-to-end without touching the real
// network.
func fixtureManager(t *testing.T) (*Manager, string) {
	t.Helper()

	tarball := makeTarGz(t, map[string]string{"plugin.json": "test"})
	digest := sha256Hex(tarball)

	// httptest server serving both /catalog.json and /linter.tgz.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			cat := Catalog{
				Version: 1,
				Plugins: []PluginEntry{{
					Name:        "linter",
					Version:     "1.0",
					Description: "linter plugin",
					TarballURL:  "http://" + r.Host + "/linter.tgz",
					SHA256:      digest,
				}},
			}
			_ = json.NewEncoder(w).Encode(cat)
		case "/linter.tgz":
			_, _ = w.Write(tarball)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	mgr := &Manager{
		Marketplace: New(srv.URL, WithHTTPClient(srv.Client())),
		Installer:   &Installer{Dir: dir, HTTPClient: srv.Client()},
		StatePath:   filepath.Join(dir, "state.json"),
	}
	return mgr, dir
}

func TestManager_InstallRecordsState(t *testing.T) {
	mgr, dir := fixtureManager(t)
	row, err := mgr.Install(context.Background(), "linter")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if row.Name != "linter" || row.Version != "1.0" {
		t.Errorf("unexpected row: %+v", row)
	}
	list, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 installed plugin, got %d", len(list))
	}
	// Plugin files should be on disk.
	if _, err := http.Dir(filepath.Join(dir, "linter")).Open("plugin.json"); err != nil {
		t.Errorf("plugin file missing: %v", err)
	}
	// State file should exist.
	if _, err := http.Dir(dir).Open("state.json"); err != nil {
		t.Errorf("state file missing: %v", err)
	}
}

func TestManager_InstallReplacesExisting(t *testing.T) {
	mgr, _ := fixtureManager(t)
	if _, err := mgr.Install(context.Background(), "linter"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := mgr.Install(context.Background(), "linter"); err != nil {
		t.Fatalf("second install: %v", err)
	}
	list, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 row after re-install, got %d", len(list))
	}
}

func TestManager_UninstallClearsState(t *testing.T) {
	mgr, dir := fixtureManager(t)
	if _, err := mgr.Install(context.Background(), "linter"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := mgr.Uninstall(context.Background(), "linter"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := http.Dir(filepath.Join(dir, "linter")).Open("plugin.json"); err == nil {
		t.Errorf("expected plugin dir gone after uninstall")
	}
	list, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty state after uninstall, got %d", len(list))
	}
	// Idempotent.
	if err := mgr.Uninstall(context.Background(), "linter"); err != nil {
		t.Errorf("expected idempotent uninstall, got %v", err)
	}
}

func TestManager_InstallRejectsUnknownPlugin(t *testing.T) {
	mgr, _ := fixtureManager(t)
	if _, err := mgr.Install(context.Background(), "ghost"); err == nil {
		t.Fatal("expected error for unknown plugin")
	}
}

func TestManager_ConcurrentInstall_SerializeOnMutex(t *testing.T) {
	mgr, _ := fixtureManager(t)
	const goroutines = 5

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mgr.Install(context.Background(), "linter"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent install returned error: %v", err)
	}

	list, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected exactly 1 installed plugin after %d concurrent installs, got %d", goroutines, len(list))
	}
}

func TestManager_SearchDelegates(t *testing.T) {
	mgr, _ := fixtureManager(t)
	hits, err := mgr.Search(context.Background(), "lint")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "linter" {
		t.Errorf("expected linter hit, got %+v", hits)
	}
}
