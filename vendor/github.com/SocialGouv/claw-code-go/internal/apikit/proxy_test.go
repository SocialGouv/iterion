package apikit

import (
	"testing"
)

func configFromMap(pairs map[string]string) ProxyConfig {
	return proxyConfigFromLookup(func(key string) string {
		return pairs[key]
	})
}

func TestProxyConfigIsEmptyWhenNoVarsSet(t *testing.T) {
	config := configFromMap(map[string]string{})
	if !config.IsEmpty() {
		t.Error("empty config should report IsEmpty")
	}
	if config.HTTPProxy != "" || config.HTTPSProxy != "" || config.NoProxy != "" {
		t.Error("all fields should be empty")
	}
}

func TestProxyConfigReadsUppercase(t *testing.T) {
	config := configFromMap(map[string]string{
		"HTTP_PROXY":  "http://proxy.internal:3128",
		"HTTPS_PROXY": "http://secure.internal:3129",
		"NO_PROXY":    "localhost,127.0.0.1,.corp",
	})

	if config.HTTPProxy != "http://proxy.internal:3128" {
		t.Errorf("unexpected HTTP_PROXY: %s", config.HTTPProxy)
	}
	if config.HTTPSProxy != "http://secure.internal:3129" {
		t.Errorf("unexpected HTTPS_PROXY: %s", config.HTTPSProxy)
	}
	if config.NoProxy != "localhost,127.0.0.1,.corp" {
		t.Errorf("unexpected NO_PROXY: %s", config.NoProxy)
	}
	if config.IsEmpty() {
		t.Error("should not be empty")
	}
}

func TestProxyConfigFallsBackToLowercase(t *testing.T) {
	config := configFromMap(map[string]string{
		"http_proxy":  "http://lower.internal:3128",
		"https_proxy": "http://lower-secure.internal:3129",
		"no_proxy":    ".lower",
	})

	if config.HTTPProxy != "http://lower.internal:3128" {
		t.Errorf("unexpected http_proxy: %s", config.HTTPProxy)
	}
	if config.HTTPSProxy != "http://lower-secure.internal:3129" {
		t.Errorf("unexpected https_proxy: %s", config.HTTPSProxy)
	}
	if config.NoProxy != ".lower" {
		t.Errorf("unexpected no_proxy: %s", config.NoProxy)
	}
}

func TestProxyConfigPrefersUppercaseOverLowercase(t *testing.T) {
	config := configFromMap(map[string]string{
		"HTTP_PROXY": "http://upper.internal:3128",
		"http_proxy": "http://lower.internal:3128",
	})

	if config.HTTPProxy != "http://upper.internal:3128" {
		t.Errorf("should prefer uppercase, got: %s", config.HTTPProxy)
	}
}

func TestProxyConfigTreatsEmptyStringsAsUnset(t *testing.T) {
	config := configFromMap(map[string]string{
		"HTTP_PROXY": "",
		"http_proxy": "",
	})

	if config.HTTPProxy != "" {
		t.Errorf("empty string should be treated as unset, got: %s", config.HTTPProxy)
	}
}

func TestFromProxyURLSetsUnifiedField(t *testing.T) {
	config := FromProxyURL("http://unified.internal:3128")

	if config.ProxyURL != "http://unified.internal:3128" {
		t.Errorf("unexpected ProxyURL: %s", config.ProxyURL)
	}
	if config.HTTPProxy != "" || config.HTTPSProxy != "" {
		t.Error("per-scheme fields should be empty")
	}
	if config.IsEmpty() {
		t.Error("should not be empty with ProxyURL set")
	}
}

func TestBuildHTTPClientSucceedsWithNoProxy(t *testing.T) {
	client, err := BuildHTTPClientWith(ProxyConfig{})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if client == nil {
		t.Error("client should not be nil")
	}
}

func TestBuildHTTPClientSucceedsWithValidProxies(t *testing.T) {
	config := ProxyConfig{
		HTTPProxy:  "http://proxy.internal:3128",
		HTTPSProxy: "http://secure.internal:3129",
		NoProxy:    "localhost,127.0.0.1",
	}
	client, err := BuildHTTPClientWith(config)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if client == nil {
		t.Error("client should not be nil")
	}
}

func TestBuildHTTPClientSucceedsWithUnifiedProxy(t *testing.T) {
	config := ProxyConfig{
		ProxyURL: "http://unified.internal:3128",
		NoProxy:  "localhost",
	}
	client, err := BuildHTTPClientWith(config)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if client == nil {
		t.Error("client should not be nil")
	}
}

func TestBuildHTTPClientOrDefaultReturnsClientAlways(t *testing.T) {
	// Should always return a non-nil client, even when env vars are unset
	client := BuildHTTPClientOrDefault()
	if client == nil {
		t.Error("BuildHTTPClientOrDefault should always return a non-nil client")
	}
}

func TestNoProxyExclusion(t *testing.T) {
	// Test the parsing and matching logic directly
	list := parseNoProxy("localhost,127.0.0.1,.corp")
	if len(list) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(list))
	}

	if !isNoProxy("localhost", list) {
		t.Error("localhost should be excluded")
	}
	if !isNoProxy("127.0.0.1", list) {
		t.Error("127.0.0.1 should be excluded")
	}
	if !isNoProxy("api.corp", list) {
		t.Error("api.corp should match .corp suffix")
	}
	if isNoProxy("example.com", list) {
		t.Error("example.com should not be excluded")
	}
}
