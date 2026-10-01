package sandbox

import (
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Two clients that read NO_PROXY entries as patterns, each after its source:
// Ruby 3.2's URI::Generic.use_proxy? (Net::HTTP, open-uri) and Python
// requests 2.32's should_bypass_proxies (pip vendors it). A host either one
// reads as covered goes around the egress proxy.
var rubyNoProxyEntry = regexp.MustCompile(`([^:,\s]+)(?::(\d+))?`)

func rubyBypasses(noProxy, host string, port int) bool {
	hostname := strings.ToLower(host)
	addr := net.ParseIP(host)
	for _, m := range rubyNoProxyEntry.FindAllStringSubmatch(noProxy, -1) {
		pHost, pPort := strings.ToLower(m[1]), m[2]
		if pPort != "" && pPort != strconv.Itoa(port) {
			continue
		}
		if strings.HasPrefix(pHost, ".") {
			if strings.HasSuffix(hostname, pHost) {
				return true
			}
		} else if strings.HasSuffix("."+hostname, "."+pHost) {
			return true
		}
		if addr != nil {
			if _, cidr, err := net.ParseCIDR(pHost); err == nil && cidr.Contains(addr) {
				return true
			}
			if ip := net.ParseIP(pHost); ip != nil && ip.Equal(addr) {
				return true
			}
		}
	}
	return false
}

func requestsBypasses(noProxy, host string, port int) bool {
	host = strings.ToLower(host)
	ipv4 := !strings.Contains(host, ":") && net.ParseIP(host) != nil
	for _, entry := range strings.Split(strings.ReplaceAll(noProxy, " ", ""), ",") {
		if entry == "" {
			continue
		}
		if ipv4 {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && !strings.Contains(entry, ":") {
				if cidr.Contains(net.ParseIP(host)) {
					return true
				}
			} else if host == entry {
				return true
			}
			continue
		}
		if strings.HasSuffix(host, entry) || strings.HasSuffix(host+":"+strconv.Itoa(port), entry) {
			return true
		}
	}
	return false
}

// TestLoopbackNoProxySendsNoOtherHostAroundTheProxy: no entry of the
// sandbox's NO_PROXY reads, to a client that matches entries as patterns, as
// another host — the kube API, the docker gateway, an IPv6 cluster address.
// Every entry is a plain host name or a canonical IPv4 address: a bracket, a
// colon or a slash is read as a port, a mask or a pattern by some client
// (httpx refuses to build a client on [::1]), a leading dot as a domain
// suffix, a short or odd IPv4 form as a suffix of another address.
func TestLoopbackNoProxySendsNoOtherHostAroundTheProxy(t *testing.T) {
	for _, entry := range strings.Split(LoopbackNoProxy, ",") {
		if entry == "" || strings.ContainsAny(entry, "[]:/* ") || strings.HasPrefix(entry, ".") {
			t.Errorf("NO_PROXY entry %q is not a plain host name or IPv4 address", entry)
		}
		if strings.Trim(entry, "0123456789.") == "" {
			if ip := net.ParseIP(entry); ip == nil || ip.To4() == nil || ip.String() != entry {
				t.Errorf("NO_PROXY entry %q is no canonical IPv4 address", entry)
			}
		}
	}
	// The models keep their teeth: a bare ::1 is the misreading they catch.
	if !rubyBypasses("::1", "10.96.0.1", 8080) || !requestsBypasses("::1", "fd00:10:96::1", 8080) {
		t.Fatal("the client models no longer read a bare ::1 as a pattern")
	}
	clients := map[string]func(string, string, int) bool{"Ruby Net::HTTP": rubyBypasses, "Python requests": requestsBypasses}
	for _, host := range []string{"10.96.0.1", "10.43.0.1", "172.17.0.1", "192.168.1.1", "169.254.169.254", "1.1.1.1",
		"fd00:10:96::1", "2001:db8::1", "fe80::1", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local",
		"metadata.google.internal", "vault.vault.svc.cluster.local", "example.com"} {
		for name, bypasses := range clients {
			if bypasses(LoopbackNoProxy, host, 8080) {
				t.Errorf("%s reads NO_PROXY=%q as covering %s: it goes around the proxy", name, LoopbackNoProxy, host)
			}
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1", "0.0.0.0"} {
		for name, bypasses := range clients {
			if !bypasses(LoopbackNoProxy, host, 8080) {
				t.Errorf("%s sends %s through the proxy with NO_PROXY=%q", name, host, LoopbackNoProxy)
			}
		}
	}
}
