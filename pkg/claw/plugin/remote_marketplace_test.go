package plugin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/plugins"
)

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stubServer wires the conventional /index.json + /<name>/manifest.json +
// /<name>/<name>-<version>.tar.gz layout described in docs/plugin_signing.md.
type stubServer struct {
	*httptest.Server
	tarball []byte
	digest  string
}

func newStubServer(t *testing.T, manifest RemoteManifest, tarball []byte) *stubServer {
	t.Helper()
	digest := sha256Hex(tarball)
	manifest.SHA256 = digest

	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		idx := RemoteIndex{
			Version: 1,
			Plugins: []RemotePluginEntry{{
				Name:          manifest.Name,
				LatestVersion: manifest.Version,
				URL:           manifest.Name + "/manifest.json",
				Description:   manifest.Description,
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idx)
	})

	srv := httptest.NewServer(mux)
	tarballPath := fmt.Sprintf("/%s/%s-%s.tar.gz", manifest.Name, manifest.Name, manifest.Version)
	manifest.TarballURL = srv.URL + tarballPath

	mux.HandleFunc(fmt.Sprintf("/%s/manifest.json", manifest.Name), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifest)
	})
	mux.HandleFunc(tarballPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	})

	return &stubServer{Server: srv, tarball: tarball, digest: digest}
}

func sampleManifest() RemoteManifest {
	return RemoteManifest{
		PluginManifest: PluginManifest{
			Name:        "linter",
			Version:     "1.0.0",
			Description: "A linter plugin",
		},
	}
}

func TestNewRemoteMarketplace_RejectsHTTPByDefault(t *testing.T) {
	if _, err := NewRemoteMarketplace("http://example.invalid"); err == nil {
		t.Fatal("expected error for plain http baseURL, got nil")
	}
	if _, err := NewRemoteMarketplace("ftp://example.invalid"); err == nil {
		t.Fatal("expected error for ftp baseURL, got nil")
	}
	if _, err := NewRemoteMarketplace(""); err == nil {
		t.Fatal("expected error for empty baseURL, got nil")
	}
}

func TestListPluginsParsesIndex(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"plugin.json": `{"name":"linter","version":"1.0.0"}`})
	srv := newStubServer(t, sampleManifest(), tarball)
	defer srv.Close()

	m, err := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}

	plugins, err := m.ListPlugins(context.Background())
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Name != "linter" || plugins[0].LatestVersion != "1.0.0" {
		t.Fatalf("unexpected plugins: %#v", plugins)
	}
}

func TestFetchManifestRoundTrip(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"plugin.json": `{"name":"linter"}`})
	srv := newStubServer(t, sampleManifest(), tarball)
	defer srv.Close()

	m, err := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}

	got, err := m.FetchManifest(context.Background(), "linter")
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if got.Name != "linter" || got.Version != "1.0.0" {
		t.Errorf("unexpected manifest: %#v", got)
	}
	if got.SHA256 != srv.digest {
		t.Errorf("sha256 not propagated: got %q want %q", got.SHA256, srv.digest)
	}
	if got.TarballURL == "" {
		t.Error("tarball url empty")
	}
	if len(got.RawJSON) == 0 {
		t.Error("RawJSON should be populated for contract validation")
	}
}

func TestFetchManifestNotFound(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"plugin.json": `{"name":"linter"}`})
	srv := newStubServer(t, sampleManifest(), tarball)
	defer srv.Close()

	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()))

	_, err := m.FetchManifest(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	var pe *PluginError
	if !errors.As(err, &pe) || pe.Kind != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInstallExtractsTarballAndVerifiesSHA(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{
		"plugin.json":    `{"name":"linter","version":"1.0.0"}`,
		"bin/run.sh":     "#!/bin/sh\necho hi\n",
		"docs/readme.md": "doc",
	})
	srv := newStubServer(t, sampleManifest(), tarball)
	defer srv.Close()

	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()))

	dest := t.TempDir()
	manifest, err := m.Install(context.Background(), "linter", dest)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if manifest.Name != "linter" {
		t.Errorf("manifest.Name: got %q want linter", manifest.Name)
	}
	for _, want := range []string{"plugin.json", "bin/run.sh", "docs/readme.md"} {
		if _, err := os.Stat(filepath.Join(dest, "linter", want)); err != nil {
			t.Errorf("expected %s to exist after install: %v", want, err)
		}
	}
}

func TestInstallRejectsBadSHA256(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"plugin.json": `{"name":"linter"}`})
	manifest := sampleManifest()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tarballPath := "/linter/linter-1.0.0.tar.gz"
	manifest.TarballURL = srv.URL + tarballPath
	manifest.SHA256 = strings.Repeat("0", 64) // valid hex shape, wrong digest

	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		idx := RemoteIndex{Plugins: []RemotePluginEntry{{
			Name: manifest.Name, LatestVersion: manifest.Version, URL: "linter/manifest.json",
		}}}
		_ = json.NewEncoder(w).Encode(idx)
	})
	mux.HandleFunc("/linter/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(manifest)
	})
	mux.HandleFunc(tarballPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	})

	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()))

	_, err := m.Install(context.Background(), "linter", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum error, got %v", err)
	}
}

func TestInstallVerifiesSignature(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"plugin.json": `{"name":"signed"}`})
	manifest := sampleManifest()
	manifest.Name = "signed"
	manifest.SignatureBundle = `{"mediaType":"application/vnd.dev.sigstore.bundle+json"}`
	srv := newStubServer(t, manifest, tarball)
	defer srv.Close()

	stub := &stubVerifier{accept: true}
	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()),
		WithMarketplaceVerifier(stub))

	if _, err := m.Install(context.Background(), "signed", t.TempDir()); err != nil {
		t.Fatalf("Install with valid signature: %v", err)
	}
	if !stub.called {
		t.Fatal("verifier was not invoked despite signature material")
	}
}

func TestInstallRejectsBadSignature(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"plugin.json": `{"name":"signed"}`})
	manifest := sampleManifest()
	manifest.Name = "signed"
	manifest.SignatureBundle = `{"mediaType":"application/vnd.dev.sigstore.bundle+json"}`
	srv := newStubServer(t, manifest, tarball)
	defer srv.Close()

	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()),
		WithMarketplaceVerifier(&stubVerifier{accept: false, reason: "bad sig"}))

	_, err := m.Install(context.Background(), "signed", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "bad sig") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestInstallRequireSignedRejectsMissingMaterial(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"plugin.json": `{"name":"linter"}`})
	srv := newStubServer(t, sampleManifest(), tarball)
	defer srv.Close()

	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()),
		WithMarketplaceRequireSigned(true))

	_, err := m.Install(context.Background(), "linter", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected RequireSigned error, got %v", err)
	}
}

func TestInstallContextCancellable(t *testing.T) {
	// Server hangs on tarball download; cancelling the context must
	// propagate and abort the install promptly. This guards against the
	// install path swallowing context.Done — relevant after the recent
	// hook-cancel fix.
	gate := make(chan struct{})
	defer close(gate)

	manifest := sampleManifest()
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		idx := RemoteIndex{Plugins: []RemotePluginEntry{{
			Name: manifest.Name, LatestVersion: manifest.Version, URL: "linter/manifest.json",
		}}}
		_ = json.NewEncoder(w).Encode(idx)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tarballPath := "/linter/linter-1.0.0.tar.gz"
	manifest.TarballURL = srv.URL + tarballPath
	manifest.SHA256 = sha256Hex([]byte("doesn't matter, never reaches sha"))
	mux.HandleFunc("/linter/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(manifest)
	})
	mux.HandleFunc(tarballPath, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Install(ctx, "linter", t.TempDir())
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after cancel, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("install did not honor context cancel within 2s")
	}
}

func TestNetworkErrorPropagated(t *testing.T) {
	hits := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream offline"))
	}))
	defer srv.Close()

	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()))

	_, err := m.ListPlugins(context.Background())
	if err == nil {
		t.Fatal("expected error from 502 response, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("expected 502 in error, got %v", err)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Error("server was never reached")
	}
}

func TestInstallRejectsInsecureTarballWhenStrict(t *testing.T) {
	// BaseURL HTTPS-allowed via httptest.NewTLSServer, but the manifest
	// claims an http:// tarball — install must refuse without the
	// insecure flag.
	tarball := makeTarGz(t, map[string]string{"plugin.json": `{"name":"linter"}`})
	manifest := sampleManifest()
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	manifest.TarballURL = "http://example.invalid/linter.tar.gz"
	manifest.SHA256 = sha256Hex(tarball)
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		idx := RemoteIndex{Plugins: []RemotePluginEntry{{
			Name: manifest.Name, LatestVersion: manifest.Version, URL: "linter/manifest.json",
		}}}
		_ = json.NewEncoder(w).Encode(idx)
	})
	mux.HandleFunc("/linter/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(manifest)
	})

	m, _ := NewRemoteMarketplace(srv.URL, WithMarketplaceHTTPClient(srv.Client()))
	_, err := m.Install(context.Background(), "linter", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("expected insecure-scheme rejection, got %v", err)
	}
}

func TestMetadataSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Stream more bytes than the cap.
		junk := strings.Repeat("x", int(maxMetadataBytes)+1024)
		_, _ = w.Write([]byte(`{"version":1,"plugins":[],"junk":"` + junk + `"}`))
	}))
	defer srv.Close()

	m, _ := NewRemoteMarketplace(srv.URL,
		WithMarketplaceAllowInsecure(true),
		WithMarketplaceHTTPClient(srv.Client()))

	_, err := m.ListPlugins(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
}

// stubVerifier is a deterministic SignatureVerifier for tests.
type stubVerifier struct {
	accept bool
	reason string
	called bool
}

func (s *stubVerifier) Verify(ctx context.Context, tarball []byte, entry plugins.PluginEntry) error {
	s.called = true
	if s.accept {
		return nil
	}
	if s.reason == "" {
		s.reason = "stub rejected"
	}
	return errors.New(s.reason)
}
