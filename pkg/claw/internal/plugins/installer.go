package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// maxTarballBytes caps download AND cumulative extraction size so a
// hostile catalog can't OOM or fill the disk. 64 MB is generous for
// plugin tarballs (much bigger than any reasonable bundle of
// TS/Go/Python code). Declared as a var rather than a const so tests
// can lower it without realistically allocating 64 MB of fixture data.
var maxTarballBytes int64 = 64 * 1024 * 1024

// Installer materializes plugins from PluginEntry tarballs into a
// per-plugin directory under Dir. Each plugin lives in
// <Dir>/<plugin-name> and is replaced atomically on reinstall.
type Installer struct {
	Dir        string
	HTTPClient *http.Client

	// Verifier validates an optional cosign signature on the tarball
	// after the SHA-256 check. When nil, signature fields on the
	// entry are ignored unless RequireSigned is set (in which case
	// the install fails up-front).
	Verifier SignatureVerifier

	// RequireSigned forces every install to carry a verified
	// signature. Entries without signature material are rejected
	// with a clear error. Operators set this via env
	// CLAW_REQUIRE_SIGNED=1 (see Manager) or wire it directly when
	// constructing the installer programmatically.
	RequireSigned bool
}

// NewInstaller constructs an installer with sensible defaults.
func NewInstaller(dir string) *Installer {
	return &Installer{Dir: dir, HTTPClient: http.DefaultClient}
}

// Install downloads entry.TarballURL, verifies its SHA-256 against
// entry.SHA256, optionally checks a cosign signature when configured,
// and extracts it into <Dir>/<entry.Name>. The extraction is performed
// via a temp directory + atomic rename so partial failures never leave
// a half-installed plugin in place.
func (i *Installer) Install(ctx context.Context, entry PluginEntry) error {
	if i.Dir == "" {
		return errors.New("installer: Dir is empty")
	}
	if entry.Name == "" {
		return errors.New("installer: entry.Name is empty")
	}
	if entry.TarballURL == "" || entry.SHA256 == "" {
		return errors.New("installer: entry missing TarballURL or SHA256")
	}

	if err := os.MkdirAll(i.Dir, 0o755); err != nil {
		return fmt.Errorf("installer: mkdir root: %w", err)
	}

	body, err := i.downloadAndVerify(ctx, entry)
	if err != nil {
		return err
	}

	// Signature gate: enforce RequireSigned, then run the verifier
	// when material is present. Failures here abort before extraction.
	if i.RequireSigned && !entry.hasSignatureFields() {
		return fmt.Errorf("installer: plugin %q has no signature material but RequireSigned is set", entry.Name)
	}
	if entry.hasSignatureFields() || i.RequireSigned {
		v := i.Verifier
		if v == nil {
			v = unconfiguredVerifier{}
		}
		if err := v.Verify(ctx, body, entry); err != nil {
			return fmt.Errorf("installer: signature verification failed for %q: %w", entry.Name, err)
		}
	}

	stagingDir, err := os.MkdirTemp(i.Dir, ".staging-"+entry.Name+"-")
	if err != nil {
		return fmt.Errorf("installer: mktemp: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stagingDir) }

	if err := extractTarGz(body, stagingDir); err != nil {
		cleanup()
		return err
	}

	target := filepath.Join(i.Dir, entry.Name)
	// If a previous version exists, swap it aside so we can roll back
	// on rename failure (Windows in particular doesn't allow renaming
	// over a non-empty directory).
	backup := target + ".old"
	hadExisting := false
	if _, err := os.Stat(target); err == nil {
		hadExisting = true
		if err := os.Rename(target, backup); err != nil {
			cleanup()
			return fmt.Errorf("installer: backup existing: %w", err)
		}
	}

	if err := os.Rename(stagingDir, target); err != nil {
		// Restore previous version if rename failed.
		if hadExisting {
			_ = os.Rename(backup, target)
		}
		cleanup()
		return fmt.Errorf("installer: install: %w", err)
	}
	if hadExisting {
		_ = os.RemoveAll(backup)
	}
	return nil
}

// Uninstall removes <Dir>/<name>. A missing target is not an error so
// callers can be idempotent.
func (i *Installer) Uninstall(ctx context.Context, name string) error {
	if i.Dir == "" || name == "" {
		return errors.New("installer: Dir and name are required")
	}
	target := filepath.Join(i.Dir, name)
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("installer: remove %s: %w", target, err)
	}
	return nil
}

func (i *Installer) downloadAndVerify(ctx context.Context, entry PluginEntry) ([]byte, error) {
	client := i.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.TarballURL, nil)
	if err != nil {
		return nil, fmt.Errorf("installer: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("installer: download %s: %w", entry.TarballURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("installer: %s returned %d", entry.TarballURL, resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxTarballBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("installer: read body: %w", err)
	}
	if int64(len(body)) > maxTarballBytes {
		return nil, fmt.Errorf("installer: tarball exceeds %d-byte cap", maxTarballBytes)
	}

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	want := strings.TrimPrefix(strings.ToLower(entry.SHA256), "sha256:")
	if got != want {
		return nil, fmt.Errorf("installer: checksum mismatch for %s: got %s want %s", entry.Name, got, want)
	}
	return body, nil
}

// extractTarGz unpacks a gzipped tar archive into dest. Path traversal
// (../) and absolute paths are rejected — every entry must resolve
// inside dest. Symlinks are skipped entirely; we don't follow them and
// we don't recreate them.
//
// The total bytes written across all extracted files is capped at
// maxTarballBytes — a malicious tarball with N small headers each
// claiming 64 MB of content can no longer chew through disk by
// stacking entries.
func extractTarGz(data []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("installer: gunzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	remaining := int64(maxTarballBytes)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("installer: tar read: %w", err)
		}

		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
			return fmt.Errorf("installer: tar path escapes archive: %q", hdr.Name)
		}
		target := filepath.Join(dest, clean)
		// Belt-and-braces: ensure the resolved target is still inside dest.
		rel, err := filepath.Rel(dest, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("installer: tar path escapes archive: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("installer: mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("installer: mkdir parent: %w", err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777|0o600)
			if err != nil {
				return fmt.Errorf("installer: create %s: %w", target, err)
			}
			// Copy at most `remaining+1` so we can detect tarballs that
			// would push us past the cap and abort instead of silently
			// truncating.
			n, err := io.Copy(f, io.LimitReader(tr, remaining+1))
			f.Close()
			if err != nil {
				return fmt.Errorf("installer: write %s: %w", target, err)
			}
			remaining -= n
			if remaining < 0 {
				return fmt.Errorf("installer: extracted contents exceed %d-byte cap", maxTarballBytes)
			}
		default:
			// Skip symlinks, char devices, FIFOs, etc.
		}
	}
}
