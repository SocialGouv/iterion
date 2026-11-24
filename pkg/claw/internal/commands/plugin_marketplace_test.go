package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/plugins"
)

type fakePluginProvider struct {
	mgr *plugins.Manager
}

func (f fakePluginProvider) PluginManager() *plugins.Manager { return f.mgr }

func TestPluginsCommand_DispatchesSubcommands(t *testing.T) {
	// Build a minimal tarball + catalog server.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "x", Mode: 0o644, Size: 4}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write([]byte("data"))
	_ = tw.Close()
	_ = gz.Close()
	tarball := buf.Bytes()
	sum := sha256.Sum256(tarball)
	digest := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			cat := plugins.Catalog{
				Version: 1,
				Plugins: []plugins.PluginEntry{{
					Name:        "alpha",
					Version:     "1.0",
					Description: "alpha plugin",
					TarballURL:  "http://" + r.Host + "/alpha.tgz",
					SHA256:      digest,
				}},
			}
			_ = json.NewEncoder(w).Encode(cat)
		case "/alpha.tgz":
			_, _ = w.Write(tarball)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	mgr := &plugins.Manager{
		Marketplace: plugins.New(srv.URL, plugins.WithHTTPClient(srv.Client())),
		Installer:   &plugins.Installer{Dir: dir, HTTPClient: srv.Client()},
		StatePath:   filepath.Join(dir, "state.json"),
	}

	r := NewRegistry()
	RegisterPluginMarketplaceCommands(r)
	provider := fakePluginProvider{mgr: mgr}

	// Search should find alpha.
	if _, err := r.Execute("/store search alpha", provider); err != nil {
		t.Errorf("/store search: %v", err)
	}

	// Install should succeed and persist state.
	if _, err := r.Execute("/store install alpha", provider); err != nil {
		t.Errorf("/store install: %v", err)
	}
	list, err := mgr.List()
	if err != nil || len(list) != 1 {
		t.Errorf("after install, expected 1 plugin, got list=%v err=%v", list, err)
	}

	// Uninstall should drop it.
	if _, err := r.Execute("/store uninstall alpha", provider); err != nil {
		t.Errorf("/store uninstall: %v", err)
	}
	list, _ = mgr.List()
	if len(list) != 0 {
		t.Errorf("expected empty list after uninstall, got %d", len(list))
	}
}

func TestPluginsCommand_NoProvider(t *testing.T) {
	r := NewRegistry()
	RegisterPluginMarketplaceCommands(r)
	// loop does not implement PluginManagerProvider — should print and not error.
	if _, err := r.Execute("/store list", struct{}{}); err != nil {
		t.Errorf("expected nil error when provider missing, got %v", err)
	}
}
