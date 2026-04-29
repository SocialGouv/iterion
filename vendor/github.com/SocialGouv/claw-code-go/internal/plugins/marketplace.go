// Package plugins implements the plugin lifecycle: discovery via a
// remote marketplace, fetch + checksum verification, install/uninstall
// state on disk, and the slash commands that drive it.
//
// This package is deliberately decoupled from the in-process plugin
// runtime (internal/plugin). It only manages the metadata + tarball +
// extraction lifecycle. Loading the extracted plugin and registering
// its tools/hooks is the job of the runtime side.
package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// PluginEntry is one row in a marketplace catalog. The TarballURL +
// SHA256 fields are the install contract: the installer only proceeds
// when the downloaded bytes hash to the announced digest.
//
// SignatureURL / SignatureBundle / CertificateIdentity /
// CertificateOIDCIssuer carry an optional cosign-style detached
// signature. When present (or when the operator sets
// CLAW_REQUIRE_SIGNED=1), the installer dispatches to a
// SignatureVerifier — see verifier.go and Installer.Verifier.
// The default verifier shells out to the `cosign` CLI; auto-detection
// picks keyless mode when a certificate field is set, key-based mode
// otherwise (configured via CLAW_PLUGIN_PUBLIC_KEY).
type PluginEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	TarballURL  string `json:"tarball_url"`
	SHA256      string `json:"sha256"`

	// SignatureURL points at a detached cosign signature for the
	// tarball (raw .sig or bundle .json). Verifier uses it to fetch
	// the signature payload before invoking cosign. SignatureBundle
	// wins when both are set.
	SignatureURL string `json:"signature_url,omitempty"`

	// SignatureBundle is the inline cosign signature bundle when the
	// catalog author prefers one document over a separate URL.
	// Either field is enough to trigger verification.
	SignatureBundle string `json:"signature_bundle,omitempty"`

	// CertificateIdentity pins the expected signer identity for
	// keyless cosign verification (e.g. an email or workflow URI).
	// Presence of this OR CertificateOIDCIssuer selects keyless mode.
	CertificateIdentity string `json:"certificate_identity,omitempty"`

	// CertificateOIDCIssuer pins the expected OIDC issuer URL.
	// Presence of this OR CertificateIdentity selects keyless mode.
	CertificateOIDCIssuer string `json:"certificate_oidc_issuer,omitempty"`

	// Homepage and License are advisory metadata shown in /plugin search
	// output. They have no effect on installation.
	Homepage string `json:"homepage,omitempty"`
	License  string `json:"license,omitempty"`
}

// hasSignatureFields reports whether the entry declares any cosign
// signature or identity-pin fields. Installer.Install consults this
// to decide whether to invoke the SignatureVerifier; Marketplace.Fetch
// uses it to surface a one-line summary of what fraction of the
// catalog is signed.
func (p PluginEntry) hasSignatureFields() bool {
	return p.SignatureURL != "" ||
		p.SignatureBundle != "" ||
		p.CertificateIdentity != "" ||
		p.CertificateOIDCIssuer != ""
}

// Catalog is the JSON shape returned by the marketplace endpoint. The
// version field is informational; clients tolerate unknown fields.
type Catalog struct {
	Version int           `json:"version"`
	Plugins []PluginEntry `json:"plugins"`
}

// Marketplace is a stateless HTTP client that knows how to fetch and
// search a plugin catalog. The zero value is unusable; construct via
// New.
type Marketplace struct {
	baseURL    string
	httpClient *http.Client
	userAgent  string
	logger     io.Writer
}

// MarketplaceOption configures Marketplace.
type MarketplaceOption func(*Marketplace)

// WithHTTPClient overrides the default HTTP client. Tests inject
// httptest.Server clients.
func WithHTTPClient(c *http.Client) MarketplaceOption {
	return func(m *Marketplace) { m.httpClient = c }
}

// WithUserAgent customizes the User-Agent header. Defaults to
// "claw-code-go-plugins/1".
func WithUserAgent(ua string) MarketplaceOption {
	return func(m *Marketplace) { m.userAgent = ua }
}

// WithMarketplaceLogger sets the writer used for non-error advisories
// (e.g. unverified signature warnings). Defaults to os.Stderr; pass
// io.Discard to silence.
func WithMarketplaceLogger(w io.Writer) MarketplaceOption {
	return func(m *Marketplace) {
		if w != nil {
			m.logger = w
		}
	}
}

// New constructs a Marketplace pointed at baseURL. The catalog is
// expected at <baseURL>/catalog.json — that path is appended unless
// baseURL already ends in .json.
func New(baseURL string, opts ...MarketplaceOption) *Marketplace {
	m := &Marketplace{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		userAgent:  "claw-code-go-plugins/1",
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *Marketplace) log(format string, args ...any) {
	w := m.logger
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "[plugins] "+format+"\n", args...)
}

// Fetch downloads and decodes the marketplace catalog. The returned
// Catalog is sorted by plugin name for stable presentation.
func (m *Marketplace) Fetch(ctx context.Context) (*Catalog, error) {
	if m.baseURL == "" {
		return nil, errors.New("marketplace: baseURL is empty")
	}
	url := m.catalogURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("marketplace: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", m.userAgent)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("marketplace: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("marketplace: %s returned %d: %s", url, resp.StatusCode, string(body))
	}

	var cat Catalog
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		return nil, fmt.Errorf("marketplace: decode catalog: %w", err)
	}
	sort.Slice(cat.Plugins, func(i, j int) bool {
		return cat.Plugins[i].Name < cat.Plugins[j].Name
	})

	// Summarise signed coverage once per Fetch so operators see at a
	// glance whether the catalog is signing-aware. The actual
	// verification happens inside Installer.Install via the
	// configured SignatureVerifier.
	var signed []string
	for _, p := range cat.Plugins {
		if p.hasSignatureFields() {
			signed = append(signed, p.Name)
		}
	}
	if len(signed) > 0 {
		m.log("INFO: %d/%d plugin(s) carry cosign signature material — verification runs at install time. Signed: %s",
			len(signed), len(cat.Plugins), strings.Join(signed, ", "))
	}

	return &cat, nil
}

// Search fetches the catalog and returns entries whose name or
// description contains query (case-insensitive). An empty query
// returns the whole catalog.
func (m *Marketplace) Search(ctx context.Context, query string) ([]PluginEntry, error) {
	cat, err := m.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return cat.Plugins, nil
	}
	var out []PluginEntry
	for _, p := range cat.Plugins {
		if strings.Contains(strings.ToLower(p.Name), q) ||
			strings.Contains(strings.ToLower(p.Description), q) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Get fetches the catalog and returns the entry matching name. An
// empty PluginEntry and (false, nil) come back when the name is
// unknown so callers can distinguish "not found" from network errors.
func (m *Marketplace) Get(ctx context.Context, name string) (PluginEntry, bool, error) {
	cat, err := m.Fetch(ctx)
	if err != nil {
		return PluginEntry{}, false, err
	}
	for _, p := range cat.Plugins {
		if p.Name == name {
			return p, true, nil
		}
	}
	return PluginEntry{}, false, nil
}

func (m *Marketplace) catalogURL() string {
	if strings.HasSuffix(m.baseURL, ".json") {
		return m.baseURL
	}
	return m.baseURL + "/catalog.json"
}
