package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTarGz produces a gzipped tar archive containing the given files
// (path → content). Used to feed the installer in tests.
func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
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

func TestInstaller_DownloadVerifyExtract(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{
		"manifest.json":  `{"name":"linter"}`,
		"bin/run.sh":     "#!/bin/sh\necho hi\n",
		"docs/readme.md": "doc",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, HTTPClient: srv.Client()}

	entry := PluginEntry{
		Name:       "linter",
		TarballURL: srv.URL + "/linter.tgz",
		SHA256:     sha256Hex(tarball),
	}
	if err := inst.Install(context.Background(), entry); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, want := range []string{"manifest.json", "bin/run.sh", "docs/readme.md"} {
		path := filepath.Join(dir, "linter", want)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to exist after install: %v", path, err)
		}
	}
}

func TestInstaller_RejectsBadChecksum(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"file": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, HTTPClient: srv.Client()}
	entry := PluginEntry{
		Name:       "linter",
		TarballURL: srv.URL + "/x.tgz",
		SHA256:     "deadbeef",
	}
	err := inst.Install(context.Background(), entry)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "linter")); !os.IsNotExist(err) {
		t.Errorf("expected target dir to be absent, got err=%v", err)
	}
}

func TestInstaller_RejectsPathTraversal(t *testing.T) {
	// Build a tarball with a malicious entry name.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "../escape.txt", Mode: 0o644, Size: 4}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write([]byte("evil"))
	_ = tw.Close()
	_ = gz.Close()
	tarball := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, HTTPClient: srv.Client()}
	entry := PluginEntry{
		Name:       "evil",
		TarballURL: srv.URL,
		SHA256:     sha256Hex(tarball),
	}
	err := inst.Install(context.Background(), entry)
	if err == nil || !strings.Contains(err.Error(), "escape") {
		t.Fatalf("expected path-escape error, got %v", err)
	}
}

func TestInstaller_Uninstall(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "linter")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "x"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := &Installer{Dir: dir}
	if err := inst.Uninstall(context.Background(), "linter"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("expected target dir gone, got err=%v", err)
	}
	// Idempotent.
	if err := inst.Uninstall(context.Background(), "linter"); err != nil {
		t.Errorf("expected idempotent uninstall, got %v", err)
	}
}

func TestInstaller_RejectsCumulativeOversize(t *testing.T) {
	// A tarball with two entries that together exceed the cap. We
	// shrink the cap to 60 KB for the test and ship 2x40 KB files —
	// download passes (80 KB ≤ 60 KB+1 buffer), checksum passes, but
	// the cumulative extraction must abort partway through.
	saved := maxTarballBytes
	defer func() { maxTarballBytes = saved }()
	maxTarballBytes = 100 * 1024

	chunk := strings.Repeat("X", 40*1024)
	tarball := makeTarGz(t, map[string]string{
		"a.bin": chunk,
		"b.bin": chunk,
		"c.bin": chunk, // 3×40 KB = 120 KB > 100 KB cap
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, HTTPClient: srv.Client()}
	err := inst.Install(context.Background(), PluginEntry{
		Name:       "huge",
		TarballURL: srv.URL,
		SHA256:     sha256Hex(tarball),
	})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "huge")); !os.IsNotExist(err) {
		t.Errorf("expected target dir absent after over-cap install, got err=%v", err)
	}
}

func TestInstaller_RejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	inst := &Installer{Dir: t.TempDir(), HTTPClient: srv.Client()}
	err := inst.Install(context.Background(), PluginEntry{
		Name:       "x",
		TarballURL: srv.URL,
		SHA256:     "deadbeef",
	})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected 503 error, got %v", err)
	}
}

func TestInstaller_RejectsEmptyEntry(t *testing.T) {
	inst := &Installer{Dir: t.TempDir()}
	if err := inst.Install(context.Background(), PluginEntry{}); err == nil {
		t.Error("expected error on empty entry")
	}
}

func TestInstaller_RejectsCorruptGzip(t *testing.T) {
	body := []byte("not a gzip stream at all")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	inst := &Installer{Dir: t.TempDir(), HTTPClient: srv.Client()}
	err := inst.Install(context.Background(), PluginEntry{
		Name:       "broken",
		TarballURL: srv.URL,
		SHA256:     sha256Hex(body),
	})
	if err == nil || !strings.Contains(err.Error(), "gunzip") {
		t.Fatalf("expected gunzip error, got %v", err)
	}
}

func TestInstaller_HandlesEmptyTarball(t *testing.T) {
	// An archive with zero entries is valid — extract should succeed
	// and produce an empty plugin directory.
	tarball := makeTarGz(t, map[string]string{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, HTTPClient: srv.Client()}
	err := inst.Install(context.Background(), PluginEntry{
		Name:       "empty",
		TarballURL: srv.URL,
		SHA256:     sha256Hex(tarball),
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "empty")); err != nil {
		t.Errorf("expected empty plugin dir to exist, got %v", err)
	}
}

func TestInstaller_AcceptsSHA256Prefix(t *testing.T) {
	// Some catalogs publish digests as "sha256:abc..." while others
	// emit raw hex. Both must work.
	tarball := makeTarGz(t, map[string]string{"x": "y"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	for _, prefix := range []string{"", "sha256:", "SHA256:"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			dir := t.TempDir()
			inst := &Installer{Dir: dir, HTTPClient: srv.Client()}
			err := inst.Install(context.Background(), PluginEntry{
				Name:       "p",
				TarballURL: srv.URL,
				SHA256:     prefix + sha256Hex(tarball),
			})
			if err != nil {
				t.Fatalf("expected install to accept %q prefix, got %v", prefix, err)
			}
		})
	}
}

func TestInstaller_ReinstallReplacesExisting(t *testing.T) {
	// First install drops manifest.json with content "v1"; second install
	// drops a different file "v2" entirely. After the second install, v1
	// must be gone.
	v1 := makeTarGz(t, map[string]string{"manifest.json": "v1"})
	v2 := makeTarGz(t, map[string]string{"different.json": "v2"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.tgz":
			_, _ = w.Write(v1)
		case "/v2.tgz":
			_, _ = w.Write(v2)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, HTTPClient: srv.Client()}

	if err := inst.Install(context.Background(), PluginEntry{
		Name: "p", TarballURL: srv.URL + "/v1.tgz", SHA256: sha256Hex(v1),
	}); err != nil {
		t.Fatalf("v1 install: %v", err)
	}
	if err := inst.Install(context.Background(), PluginEntry{
		Name: "p", TarballURL: srv.URL + "/v2.tgz", SHA256: sha256Hex(v2),
	}); err != nil {
		t.Fatalf("v2 install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "p", "manifest.json")); !os.IsNotExist(err) {
		t.Errorf("expected v1 manifest.json gone after reinstall, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p", "different.json")); err != nil {
		t.Errorf("expected v2 different.json present, got %v", err)
	}
}

func TestInstaller_RejectsCheckSumPrefixWithJunk(t *testing.T) {
	// "sha256:" prefix is allowed; anything else after the colon
	// should still be compared as hex and fail when wrong.
	tarball := makeTarGz(t, map[string]string{"x": "y"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	inst := &Installer{Dir: t.TempDir(), HTTPClient: srv.Client()}
	err := inst.Install(context.Background(), PluginEntry{
		Name: "p", TarballURL: srv.URL,
		SHA256: "sha256:" + strings.Repeat("0", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum error, got %v", err)
	}
}
