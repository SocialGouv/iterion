// Package plugin: remote marketplace fetch + verify.
//
// RemoteMarketplace exposes a small public API on top of the internal
// marketplace + installer plumbing in internal/plugins. The contract is
// HTTPS by default, an index.json advertising plugins, a per-plugin
// manifest.json that conforms to plugin.PluginManifest, and a
// SHA-256 + cosign verified tarball. The actual download / hash /
// signature / extraction chain is reused from internal/plugins so we
// don't fork the verification logic.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/internal/plugins"
)

// Default size cap for marketplace metadata documents (index.json,
// per-plugin manifest.json). 1 MiB is far above any realistic plugin
// catalog and protects against a hostile server streaming until OOM.
const maxMetadataBytes int64 = 1 * 1024 * 1024

// Default timeout for one HTTP exchange against the marketplace.
const defaultMarketplaceTimeout = 30 * time.Second

// RemotePluginEntry is one row of the marketplace index.
//
// The index advertises the plugins available; clients then fetch the
// per-plugin manifest.json (PluginManifest) for the full schema and
// install via the tarball pointed at by the manifest. Keeping the index
// minimal lets a marketplace publish hundreds of plugins without
// shipping every full manifest in one document.
type RemotePluginEntry struct {
	Name          string `json:"name"`
	LatestVersion string `json:"latest_version"`
	// URL points at the per-plugin manifest. It may be absolute or
	// relative to the marketplace BaseURL.
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// RemoteIndex is the JSON document at <BaseURL>/index.json.
type RemoteIndex struct {
	Version int                 `json:"version"`
	Plugins []RemotePluginEntry `json:"plugins"`
}

// RemoteManifest extends plugin.PluginManifest with the install
// coordinates a remote marketplace needs: where the tarball lives, the
// expected SHA-256, and optional cosign signature material that mirrors
// internal/plugins.PluginEntry. The embedded PluginManifest preserves
// the canonical plugin.json schema so a client can parse one document
// and know everything required to install + register the plugin.
type RemoteManifest struct {
	PluginManifest

	TarballURL            string `json:"tarball_url"`
	SHA256                string `json:"sha256"`
	SignatureURL          string `json:"signature_url,omitempty"`
	SignatureBundle       string `json:"signature_bundle,omitempty"`
	CertificateIdentity   string `json:"certificate_identity,omitempty"`
	CertificateOIDCIssuer string `json:"certificate_oidc_issuer,omitempty"`
}

// RemoteMarketplace is a stateless client over a remote plugin catalog.
// All install primitives — checksum verification, signature
// verification, atomic extraction — are delegated to internal/plugins
// so this façade and the existing /store slash command share one code
// path for the security-sensitive bits.
type RemoteMarketplace struct {
	BaseURL    string
	HTTPClient *http.Client

	// Verifier is the cosign signature verifier invoked on installs
	// when the manifest carries signature material (or RequireSigned
	// is set). When nil, the default plugins.CosignVerifier is wired
	// up by Install — operators can still pin a key via env
	// (CLAW_PLUGIN_PUBLIC_KEY) without configuring code.
	Verifier plugins.SignatureVerifier

	// RequireSigned forces every Install to carry a verified
	// signature; manifests without signature fields are rejected.
	RequireSigned bool

	// AllowInsecure permits http:// BaseURL / tarball / signature
	// URLs. Off by default — production deployments should serve
	// over HTTPS so a network attacker can't tamper with the tarball
	// before SHA-256 verification and offer a bogus signature.
	AllowInsecure bool
}

// MarketplaceOption configures a RemoteMarketplace at construction time.
type MarketplaceOption func(*RemoteMarketplace)

// WithMarketplaceHTTPClient overrides the default HTTP client. Tests
// inject httptest.Server.Client(); operators can plug in a client with
// a custom CA bundle or proxy.
func WithMarketplaceHTTPClient(c *http.Client) MarketplaceOption {
	return func(m *RemoteMarketplace) {
		if c != nil {
			m.HTTPClient = c
		}
	}
}

// WithMarketplaceVerifier wires a custom signature verifier. The
// default is plugins.CosignVerifier with PublicKeyFile sourced from
// CLAW_PLUGIN_PUBLIC_KEY when set.
func WithMarketplaceVerifier(v plugins.SignatureVerifier) MarketplaceOption {
	return func(m *RemoteMarketplace) {
		m.Verifier = v
	}
}

// WithMarketplaceRequireSigned forces signature verification on every
// install.
func WithMarketplaceRequireSigned(require bool) MarketplaceOption {
	return func(m *RemoteMarketplace) {
		m.RequireSigned = require
	}
}

// WithMarketplaceAllowInsecure permits http:// URLs. Intended for
// local-development scenarios where claw-store-init scaffolds a
// loopback server.
func WithMarketplaceAllowInsecure(allow bool) MarketplaceOption {
	return func(m *RemoteMarketplace) {
		m.AllowInsecure = allow
	}
}

// NewRemoteMarketplace constructs a marketplace client pointed at
// baseURL. baseURL must be HTTPS unless WithMarketplaceAllowInsecure
// is set.
func NewRemoteMarketplace(baseURL string, opts ...MarketplaceOption) (*RemoteMarketplace, error) {
	m := &RemoteMarketplace{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: defaultMarketplaceTimeout},
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.BaseURL == "" {
		return nil, &PluginError{Kind: ErrIO, Message: "marketplace: baseURL is empty"}
	}
	if err := m.checkScheme(m.BaseURL); err != nil {
		return nil, err
	}
	return m, nil
}

// ListPlugins fetches and decodes <BaseURL>/index.json.
func (m *RemoteMarketplace) ListPlugins(ctx context.Context) ([]RemotePluginEntry, error) {
	idxURL := m.BaseURL + "/index.json"
	body, err := m.fetchJSON(ctx, idxURL, maxMetadataBytes)
	if err != nil {
		return nil, err
	}
	var idx RemoteIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, &PluginError{
			Kind:    ErrJSON,
			Message: fmt.Sprintf("marketplace: decode index %s", idxURL),
			Cause:   err,
		}
	}
	return idx.Plugins, nil
}

// FetchManifest resolves name in the index then downloads the
// per-plugin manifest.json. It returns a RemoteManifest carrying both
// the canonical PluginManifest and the install coordinates.
func (m *RemoteMarketplace) FetchManifest(ctx context.Context, name string) (*RemoteManifest, error) {
	if name == "" {
		return nil, &PluginError{Kind: ErrIO, Message: "marketplace: plugin name required"}
	}
	plugins, err := m.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	var entry *RemotePluginEntry
	for i := range plugins {
		if plugins[i].Name == name {
			entry = &plugins[i]
			break
		}
	}
	if entry == nil {
		return nil, &PluginError{
			Kind:    ErrNotFound,
			Message: fmt.Sprintf("marketplace: plugin %q not found in index", name),
		}
	}

	manifestURL, err := m.resolveURL(entry.URL, name)
	if err != nil {
		return nil, err
	}
	body, err := m.fetchJSON(ctx, manifestURL, maxMetadataBytes)
	if err != nil {
		return nil, err
	}
	var rm RemoteManifest
	if err := json.Unmarshal(body, &rm); err != nil {
		return nil, &PluginError{
			Kind:    ErrJSON,
			Message: fmt.Sprintf("marketplace: decode manifest %s", manifestURL),
			Cause:   err,
		}
	}
	rm.PluginManifest.RawJSON = json.RawMessage(body)
	if rm.Name == "" {
		rm.Name = name
	}
	if rm.TarballURL == "" || rm.SHA256 == "" {
		return nil, &PluginError{
			Kind:    ErrManifestValidation,
			Message: fmt.Sprintf("marketplace: manifest for %q missing tarball_url or sha256", name),
		}
	}
	return &rm, nil
}

// Install fetches the manifest for name then downloads, verifies, and
// extracts the tarball into <dest>/<name>. The download / SHA-256 /
// cosign chain is the same one /store install uses — implemented in
// internal/plugins and reused here verbatim.
func (m *RemoteMarketplace) Install(ctx context.Context, name string, dest string) (*RemoteManifest, error) {
	if dest == "" {
		return nil, &PluginError{Kind: ErrIO, Message: "marketplace: install dest is empty"}
	}
	manifest, err := m.FetchManifest(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := m.checkScheme(manifest.TarballURL); err != nil {
		return nil, err
	}
	if manifest.SignatureURL != "" {
		if err := m.checkScheme(manifest.SignatureURL); err != nil {
			return nil, err
		}
	}

	entry := plugins.PluginEntry{
		Name:                  manifest.Name,
		Version:               manifest.Version,
		Description:           manifest.Description,
		TarballURL:            manifest.TarballURL,
		SHA256:                manifest.SHA256,
		SignatureURL:          manifest.SignatureURL,
		SignatureBundle:       manifest.SignatureBundle,
		CertificateIdentity:   manifest.CertificateIdentity,
		CertificateOIDCIssuer: manifest.CertificateOIDCIssuer,
	}

	inst := &plugins.Installer{
		Dir:           dest,
		HTTPClient:    m.HTTPClient,
		Verifier:      m.Verifier,
		RequireSigned: m.RequireSigned,
	}
	if inst.Verifier == nil {
		inst.Verifier = &plugins.CosignVerifier{HTTPClient: m.HTTPClient}
	}
	if err := inst.Install(ctx, entry); err != nil {
		return nil, &PluginError{
			Kind:    ErrCommandFailed,
			Message: fmt.Sprintf("marketplace: install %q failed", name),
			Cause:   err,
		}
	}
	return manifest, nil
}

func (m *RemoteMarketplace) fetchJSON(ctx context.Context, target string, maxBytes int64) ([]byte, error) {
	if err := m.checkScheme(target); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &PluginError{Kind: ErrIO, Message: "marketplace: build request", Cause: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "claw-code-go/marketplace")

	client := m.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &PluginError{Kind: ErrIO, Message: fmt.Sprintf("marketplace: GET %s", target), Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("marketplace: %s returned %d: %s", target, resp.StatusCode, strings.TrimSpace(string(preview))),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, &PluginError{Kind: ErrIO, Message: fmt.Sprintf("marketplace: read %s", target), Cause: err}
	}
	if int64(len(body)) > maxBytes {
		return nil, &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("marketplace: %s exceeds %d-byte cap", target, maxBytes),
		}
	}
	return body, nil
}

// resolveURL turns a manifest URL (which may be absolute or relative to
// BaseURL) into a fully-qualified URL. A relative URL like
// "my-plugin/manifest.json" resolves under BaseURL.
func (m *RemoteMarketplace) resolveURL(raw, name string) (string, error) {
	if raw == "" {
		raw = path.Join(name, "manifest.json")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", &PluginError{Kind: ErrIO, Message: fmt.Sprintf("marketplace: invalid url %q", raw), Cause: err}
	}
	if u.IsAbs() {
		return u.String(), nil
	}
	base, err := url.Parse(m.BaseURL + "/")
	if err != nil {
		return "", &PluginError{Kind: ErrIO, Message: "marketplace: invalid base url", Cause: err}
	}
	return base.ResolveReference(u).String(), nil
}

func (m *RemoteMarketplace) checkScheme(target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return &PluginError{Kind: ErrIO, Message: fmt.Sprintf("marketplace: invalid url %q", target), Cause: err}
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if m.AllowInsecure {
			return nil
		}
		return &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("marketplace: refusing insecure http:// URL %q (set AllowInsecure to override)", target),
		}
	default:
		return &PluginError{
			Kind:    ErrIO,
			Message: fmt.Sprintf("marketplace: unsupported scheme %q in %q", u.Scheme, target),
		}
	}
}

// Sentinel errors callers may use with errors.Is.
var (
	ErrManifestNotFound = errors.New("marketplace: manifest not found")
)
