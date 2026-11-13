package plugins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockVerifier records calls and returns a configurable error. It is
// the standard test seam — the production CosignVerifier shells out to
// `cosign`, which is not appropriate for unit tests.
type mockVerifier struct {
	calls   int
	lastTar []byte
	lastSig string
	err     error
}

func (m *mockVerifier) Verify(_ context.Context, tar []byte, entry PluginEntry) error {
	m.calls++
	m.lastTar = tar
	switch {
	case entry.SignatureBundle != "":
		m.lastSig = "bundle"
	case entry.SignatureURL != "":
		m.lastSig = "url"
	default:
		m.lastSig = "none"
	}
	return m.err
}

func TestInstaller_VerifiesSignatureWhenPresent(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"file.txt": "hello"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	mv := &mockVerifier{}
	inst := &Installer{
		Dir:        t.TempDir(),
		HTTPClient: srv.Client(),
		Verifier:   mv,
	}
	entry := PluginEntry{
		Name:                "p",
		TarballURL:          srv.URL,
		SHA256:              sha256Hex(tarball),
		SignatureBundle:     `{"mediaType":"application/vnd.dev.sigstore.bundle+json"}`,
		CertificateIdentity: "ci@example.com",
	}
	if err := inst.Install(context.Background(), entry); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if mv.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", mv.calls)
	}
	if mv.lastSig != "bundle" {
		t.Fatalf("verifier saw signature source %q, want bundle", mv.lastSig)
	}
	if string(mv.lastTar) != string(tarball) {
		t.Fatalf("verifier received different tarball bytes than installer downloaded")
	}
}

func TestInstaller_AbortsOnVerificationFailure(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"a.txt": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	mv := &mockVerifier{err: errors.New("bad signature")}
	inst := &Installer{
		Dir:        t.TempDir(),
		HTTPClient: srv.Client(),
		Verifier:   mv,
	}
	entry := PluginEntry{
		Name:         "p",
		TarballURL:   srv.URL,
		SHA256:       sha256Hex(tarball),
		SignatureURL: "https://example/sig",
	}
	err := inst.Install(context.Background(), entry)
	if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("expected signature verification error, got %v", err)
	}
	// Plugin directory must NOT exist after verification failure.
	if _, statErr := installedDir(inst, "p"); statErr == nil {
		t.Errorf("plugin directory was created despite verification failure")
	}
}

func TestInstaller_RequireSignedRejectsUnsigned(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"a.txt": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	inst := &Installer{
		Dir:           t.TempDir(),
		HTTPClient:    srv.Client(),
		RequireSigned: true,
		// Verifier intentionally nil — the RequireSigned check fires
		// before the verifier runs because the entry has no signature
		// fields.
	}
	entry := PluginEntry{
		Name:       "p",
		TarballURL: srv.URL,
		SHA256:     sha256Hex(tarball),
	}
	err := inst.Install(context.Background(), entry)
	if err == nil || !strings.Contains(err.Error(), "no signature material") {
		t.Fatalf("expected no-signature-material error, got %v", err)
	}
}

func TestInstaller_RequireSignedAcceptsVerifiedEntry(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{"a.txt": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	mv := &mockVerifier{}
	inst := &Installer{
		Dir:           t.TempDir(),
		HTTPClient:    srv.Client(),
		Verifier:      mv,
		RequireSigned: true,
	}
	entry := PluginEntry{
		Name:                "p",
		TarballURL:          srv.URL,
		SHA256:              sha256Hex(tarball),
		SignatureURL:        "https://example/sig",
		CertificateIdentity: "ci@example.com",
	}
	if err := inst.Install(context.Background(), entry); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if mv.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", mv.calls)
	}
}

func TestInstaller_NoVerifierFailsOpenWhenSignatureFieldsPresent(t *testing.T) {
	// When entry carries signature fields but no verifier is wired
	// AND RequireSigned is false (default), we still treat the
	// declared signature as a contract — the installer plugs in the
	// noop verifier which fails closed. This is the safe choice: a
	// catalog declaring signatures must not be installable without
	// real verification.
	tarball := makeTarGz(t, map[string]string{"a.txt": "x"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	inst := &Installer{
		Dir:        t.TempDir(),
		HTTPClient: srv.Client(),
		// Verifier left nil — installer falls back to noopVerifier.
	}
	entry := PluginEntry{
		Name:                "p",
		TarballURL:          srv.URL,
		SHA256:              sha256Hex(tarball),
		CertificateIdentity: "ci@example.com",
	}
	err := inst.Install(context.Background(), entry)
	if err == nil || !strings.Contains(err.Error(), "no verifier configured") {
		t.Fatalf("expected no-verifier error, got %v", err)
	}
}

func TestCosignVerifier_AutoDetectsKeylessVsKeyBased(t *testing.T) {
	// We don't shell out to cosign here; we only validate that the
	// verifier short-circuits with the right error before invoking
	// cosign when material is missing or mode-mismatched.
	v := &CosignVerifier{}

	t.Run("no material at all", func(t *testing.T) {
		err := v.Verify(context.Background(), []byte("blob"), PluginEntry{})
		if err == nil || !strings.Contains(err.Error(), "no signature material") {
			t.Fatalf("expected no-signature-material error, got %v", err)
		}
	})

	t.Run("keyless without signature payload", func(t *testing.T) {
		err := v.Verify(context.Background(), []byte("blob"), PluginEntry{
			CertificateIdentity:   "ci@example.com",
			CertificateOIDCIssuer: "https://example.com",
			// No SignatureBundle/SignatureURL.
		})
		if err == nil || !strings.Contains(err.Error(), "no SignatureURL or SignatureBundle") {
			t.Fatalf("expected missing-signature-payload error, got %v", err)
		}
	})

	t.Run("key-based without public key", func(t *testing.T) {
		err := v.Verify(context.Background(), []byte("blob"), PluginEntry{
			SignatureBundle: `{"mediaType":"application/vnd.dev.sigstore.bundle+json"}`,
			// No CertificateIdentity/Issuer → key-based mode.
			// And no PublicKeyPEM/File on the verifier.
		})
		if err == nil || !strings.Contains(err.Error(), "PublicKeyPEM or PublicKeyFile") {
			t.Fatalf("expected missing-public-key error, got %v", err)
		}
	})
}

// installedDir returns the per-plugin directory under the installer
// root, or an error if it does not exist. Used by tests to assert
// non-creation after a verification failure.
func installedDir(inst *Installer, name string) (string, error) {
	path := filepath.Join(inst.Dir, name)
	_, err := os.Stat(path)
	return path, err
}
