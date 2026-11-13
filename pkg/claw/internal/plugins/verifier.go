package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SignatureVerifier is the contract used by Installer to validate a
// downloaded tarball against the optional cosign-style signature
// fields on a PluginEntry. Implementations are expected to do the
// auto-detect themselves: a keyless verification when the entry
// declares CertificateIdentity or CertificateOIDCIssuer, otherwise a
// key-based one when the verifier is configured with a public key.
//
// A successful Verify returns nil. Any error means "do not install" —
// callers must surface the message and abort the install.
type SignatureVerifier interface {
	Verify(ctx context.Context, tarball []byte, entry PluginEntry) error
}

// unconfiguredVerifier fails closed so a misconfigured RequireSigned
// or a catalog declaring signatures without a verifier wired in
// produces a loud, actionable error rather than a silent pass.
type unconfiguredVerifier struct{}

func (unconfiguredVerifier) Verify(_ context.Context, _ []byte, _ PluginEntry) error {
	return errors.New("plugin signature verification: no verifier configured (set Installer.Verifier or omit RequireSigned)")
}

// CosignVerifier shells out to the `cosign` CLI to verify cosign
// signatures. It supports both flavors:
//
//   - **Keyless** (Sigstore Fulcio + Rekor): triggered when the
//     PluginEntry declares CertificateIdentity and/or
//     CertificateOIDCIssuer. The transparency log entry is checked by
//     `cosign verify-blob` automatically.
//
//   - **Key-based** (pinned public key): used when no certificate
//     fields are present and PublicKeyPEM (or PublicKeyFile) is set.
//
// Subprocess was chosen over an in-process sigstore-go binding to keep
// the dependency footprint minimal. Operators who care about signing
// already have `cosign` on PATH; everyone else can skip RequireSigned.
type CosignVerifier struct {
	// Cmd overrides the cosign binary path (default: "cosign").
	Cmd string

	// PublicKeyPEM is the PEM-encoded public key used for key-based
	// verification. Mutually optional with PublicKeyFile — if both
	// are set, PublicKeyPEM wins.
	PublicKeyPEM []byte

	// PublicKeyFile is a path to a PEM-encoded public key. Read
	// lazily at verification time so a missing file fails the
	// install with a clear message rather than at construction.
	PublicKeyFile string

	// HTTPClient fetches SignatureURL when the entry uses an
	// out-of-band signature blob. Defaults to http.DefaultClient
	// with a 15 s timeout.
	HTTPClient *http.Client

	// Timeout caps the cosign invocation. Defaults to 60 s, plenty
	// for a Rekor lookup over a slow link.
	Timeout time.Duration
}

// Verify implements SignatureVerifier.
func (v *CosignVerifier) Verify(ctx context.Context, tarball []byte, entry PluginEntry) error {
	if !entry.hasSignatureFields() && len(v.PublicKeyPEM) == 0 && v.PublicKeyFile == "" {
		return errors.New("cosign verifier: no signature material (entry declares no signature fields and no public key configured)")
	}

	// Stage tarball, signature, and (for keyless) certificate/bundle
	// files in a temp directory so cosign can read them by path.
	stage, err := os.MkdirTemp("", "claw-cosign-")
	if err != nil {
		return fmt.Errorf("cosign verifier: stage: %w", err)
	}
	defer os.RemoveAll(stage)

	blobPath := filepath.Join(stage, "blob")
	if err := os.WriteFile(blobPath, tarball, 0o600); err != nil {
		return fmt.Errorf("cosign verifier: write blob: %w", err)
	}

	sigPath, isBundle, err := v.materializeSignature(ctx, stage, entry)
	if err != nil {
		return err
	}

	args := []string{"verify-blob"}
	keyless := entry.CertificateIdentity != "" || entry.CertificateOIDCIssuer != ""
	switch {
	case keyless:
		// Keyless: identity matchers + bundle (or detached sig).
		if entry.CertificateIdentity != "" {
			args = append(args, "--certificate-identity", entry.CertificateIdentity)
		} else {
			// Sigstore requires either identity or identity-regexp
			// for keyless verification. Anything matches when the
			// catalog only pinned the issuer.
			args = append(args, "--certificate-identity-regexp", ".*")
		}
		if entry.CertificateOIDCIssuer != "" {
			args = append(args, "--certificate-oidc-issuer", entry.CertificateOIDCIssuer)
		} else {
			args = append(args, "--certificate-oidc-issuer-regexp", ".*")
		}
		if isBundle {
			args = append(args, "--bundle", sigPath)
		} else {
			args = append(args, "--signature", sigPath)
		}

	default:
		// Key-based: pinned public key + detached signature.
		keyPath, err := v.materializePublicKey(stage)
		if err != nil {
			return err
		}
		args = append(args, "--key", keyPath)
		if isBundle {
			args = append(args, "--bundle", sigPath)
		} else {
			args = append(args, "--signature", sigPath)
		}
	}
	args = append(args, blobPath)

	bin := v.Cmd
	if bin == "" {
		bin = "cosign"
	}

	timeout := v.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cosign verifier: verify-blob failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// materializeSignature stages the cosign signature payload to disk and
// reports whether it is a sigstore bundle (vs a raw detached sig).
func (v *CosignVerifier) materializeSignature(ctx context.Context, stageDir string, entry PluginEntry) (path string, isBundle bool, err error) {
	switch {
	case entry.SignatureBundle != "":
		path = filepath.Join(stageDir, "bundle.json")
		if err := os.WriteFile(path, []byte(entry.SignatureBundle), 0o600); err != nil {
			return "", false, fmt.Errorf("cosign verifier: write bundle: %w", err)
		}
		return path, true, nil

	case entry.SignatureURL != "":
		client := v.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: 15 * time.Second}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.SignatureURL, nil)
		if err != nil {
			return "", false, fmt.Errorf("cosign verifier: build signature request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", false, fmt.Errorf("cosign verifier: fetch signature: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", false, fmt.Errorf("cosign verifier: signature URL %s returned %d", entry.SignatureURL, resp.StatusCode)
		}
		// Same cap as tarballs — a hostile catalog cannot OOM us with
		// a multi-GB "signature".
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxTarballBytes+1))
		if err != nil {
			return "", false, fmt.Errorf("cosign verifier: read signature: %w", err)
		}
		if int64(len(body)) > maxTarballBytes {
			return "", false, errors.New("cosign verifier: signature payload exceeds size cap")
		}
		// Detect bundle (JSON object) vs detached signature (base64
		// or DSSE blob) by sniffing the first non-whitespace byte.
		isBundle = false
		for _, b := range body {
			if b == ' ' || b == '\n' || b == '\r' || b == '\t' {
				continue
			}
			isBundle = b == '{'
			break
		}
		name := "signature.bin"
		if isBundle {
			name = "bundle.json"
		}
		path = filepath.Join(stageDir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return "", false, fmt.Errorf("cosign verifier: write signature: %w", err)
		}
		return path, isBundle, nil

	default:
		return "", false, errors.New("cosign verifier: entry has no SignatureURL or SignatureBundle")
	}
}

func (v *CosignVerifier) materializePublicKey(stageDir string) (string, error) {
	if len(v.PublicKeyPEM) > 0 {
		path := filepath.Join(stageDir, "cosign.pub")
		if err := os.WriteFile(path, v.PublicKeyPEM, 0o600); err != nil {
			return "", fmt.Errorf("cosign verifier: write public key: %w", err)
		}
		return path, nil
	}
	if v.PublicKeyFile != "" {
		// Cosign reads the file itself; let it surface a permission
		// or missing-file error rather than racing a Stat here.
		return v.PublicKeyFile, nil
	}
	return "", errors.New("cosign verifier: key-based mode requires PublicKeyPEM or PublicKeyFile")
}
