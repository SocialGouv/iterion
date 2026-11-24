package apikit

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

var (
	httpProxyKeys  = [2]string{"HTTP_PROXY", "http_proxy"}
	httpsProxyKeys = [2]string{"HTTPS_PROXY", "https_proxy"}
	noProxyKeys    = [2]string{"NO_PROXY", "no_proxy"}
)

// ProxyConfig holds proxy settings resolved from environment or configuration.
type ProxyConfig struct {
	HTTPProxy  string // Per-scheme HTTP proxy URL
	HTTPSProxy string // Per-scheme HTTPS proxy URL
	NoProxy    string // Comma-separated list of hosts to bypass
	ProxyURL   string // Unified proxy URL (takes precedence over per-scheme)
}

// ProxyConfigFromEnv reads proxy settings from the process environment,
// honouring both upper- and lower-case spellings.
func ProxyConfigFromEnv() ProxyConfig {
	return proxyConfigFromLookup(os.Getenv)
}

// FromProxyURL creates a proxy config from a single unified URL.
func FromProxyURL(rawURL string) ProxyConfig {
	return ProxyConfig{ProxyURL: rawURL}
}

// IsEmpty reports whether no proxy is configured.
func (c ProxyConfig) IsEmpty() bool {
	return c.ProxyURL == "" && c.HTTPProxy == "" && c.HTTPSProxy == ""
}

// BuildHTTPClient returns an *http.Client configured with the proxy settings
// from the process environment.
func BuildHTTPClient() (*http.Client, error) {
	return BuildHTTPClientWith(ProxyConfigFromEnv())
}

// BuildHTTPClientOrDefault is the infallible counterpart to BuildHTTPClient.
// When the proxy configuration is malformed it falls back to a plain client
// so that callers retain the previous behaviour and the failure surfaces on
// the first outbound request instead of at construction time. Matches
// Rust's build_http_client_or_default().
func BuildHTTPClientOrDefault() *http.Client {
	client, err := BuildHTTPClient()
	if err != nil {
		return &http.Client{}
	}
	return client
}

// BuildHTTPClientWith returns an *http.Client configured with the given proxy
// settings. When no proxy is configured, the client uses Go's default
// transport (which respects standard env vars itself, but we disable that
// and use our explicit config instead).
func BuildHTTPClientWith(config ProxyConfig) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Disable Go's built-in proxy resolution — we handle it ourselves.
	transport.Proxy = nil

	httpProxyURL, httpsProxyURL := config.HTTPProxy, config.HTTPSProxy
	if config.ProxyURL != "" {
		httpProxyURL = config.ProxyURL
		httpsProxyURL = config.ProxyURL
	}

	if httpProxyURL == "" && httpsProxyURL == "" {
		return &http.Client{Transport: transport}, nil
	}

	noProxyList := parseNoProxy(config.NoProxy)

	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		host := req.URL.Hostname()
		if isNoProxy(host, noProxyList) {
			return nil, nil
		}

		var proxyStr string
		if req.URL.Scheme == "https" && httpsProxyURL != "" {
			proxyStr = httpsProxyURL
		} else if httpProxyURL != "" {
			proxyStr = httpProxyURL
		} else if httpsProxyURL != "" {
			proxyStr = httpsProxyURL
		}

		if proxyStr == "" {
			return nil, nil
		}
		return url.Parse(proxyStr)
	}

	// Validate proxy URLs eagerly
	if httpsProxyURL != "" {
		if _, err := url.Parse(httpsProxyURL); err != nil {
			return nil, &ApiError{Kind: ErrHTTP, Cause: err}
		}
	}
	if httpProxyURL != "" {
		if _, err := url.Parse(httpProxyURL); err != nil {
			return nil, &ApiError{Kind: ErrHTTP, Cause: err}
		}
	}

	return &http.Client{Transport: transport}, nil
}

// proxyConfigFromLookup reads proxy config using a lookup function (for testability).
func proxyConfigFromLookup(lookup func(string) string) ProxyConfig {
	return ProxyConfig{
		HTTPProxy:  firstNonEmpty(httpProxyKeys[:], lookup),
		HTTPSProxy: firstNonEmpty(httpsProxyKeys[:], lookup),
		NoProxy:    firstNonEmpty(noProxyKeys[:], lookup),
	}
}

func firstNonEmpty(keys []string, lookup func(string) string) string {
	for _, key := range keys {
		val := lookup(key)
		if val != "" {
			return val
		}
	}
	return ""
}

func parseNoProxy(noProxy string) []string {
	if noProxy == "" {
		return nil
	}
	parts := strings.Split(noProxy, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, strings.ToLower(p))
		}
	}
	return result
}

func isNoProxy(host string, noProxyList []string) bool {
	host = strings.ToLower(host)
	for _, entry := range noProxyList {
		if entry == host {
			return true
		}
		// Suffix match for domain patterns like ".corp"
		if strings.HasPrefix(entry, ".") && strings.HasSuffix(host, entry) {
			return true
		}
	}
	return false
}
