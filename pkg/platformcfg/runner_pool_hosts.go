package platformcfg

import (
	"context"
	"fmt"
	"golang.org/x/net/idna"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// PoolGatewayProjectionWarningCode names a non-blocking registry/boot warning.
// C1xx is exhausted; C213 is allocated without aliasing another diagnostic.
const PoolGatewayProjectionWarningCode = "C213"

type PoolHostWarning struct {
	Code    string `json:"code"`
	Pool    string `json:"pool"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

type poolHost struct {
	name, port string
	local      bool
}

// parsePoolHost only checks shape. It performs no DNS and private names are
// valid: the registry is an operator-owned cluster projection, not an SSRF
// boundary. Names/ports are canonicalized only for comparison, never storage.
func parsePoolHost(authority string) (poolHost, error) {
	if authority == "" || strings.IndexFunc(authority, unicode.IsSpace) >= 0 || strings.ContainsAny(authority, "/@?#\\") {
		return poolHost{}, fmt.Errorf("host must be a non-empty host[:port] without whitespace, credentials or a path")
	}
	name, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return poolHost{}, fmt.Errorf("IPv6 host needs closing bracket")
		}
		name = authority[1:end]
		if end+1 < len(authority) {
			if authority[end+1] != ':' {
				return poolHost{}, fmt.Errorf("invalid bracketed host suffix")
			}
			port = authority[end+2:]
			if port == "" {
				return poolHost{}, fmt.Errorf("explicit port must not be empty")
			}
		}
		addr, err := netip.ParseAddr(name)
		if err != nil || !addr.Is6() {
			return poolHost{}, fmt.Errorf("brackets require an IPv6 literal")
		}
	} else if strings.Contains(authority, ":") {
		var err error
		name, port, err = net.SplitHostPort(authority)
		if err != nil || port == "" {
			return poolHost{}, fmt.Errorf("host port must be explicit; IPv6 requires brackets")
		}
	}
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return poolHost{}, fmt.Errorf("port must be an integer from 1 to 65535")
		}
		port = strconv.FormatUint(n, 10)
	}
	if addr, err := netip.ParseAddr(name); err == nil {
		addr = addr.Unmap()
		return poolHost{name: addr.String(), port: port, local: addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsMulticast()}, nil
	}
	ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(name, "."))
	if err != nil || ascii == "" {
		return poolHost{}, fmt.Errorf("invalid DNS host")
	}
	ascii = strings.ToLower(ascii)
	if len(ascii) > 253 {
		return poolHost{}, fmt.Errorf("DNS host exceeds 253 bytes")
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return poolHost{}, fmt.Errorf("invalid DNS label")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return poolHost{}, fmt.Errorf("invalid DNS label character")
			}
		}
	}
	local := !strings.Contains(ascii, ".")
	for _, suffix := range []string{"localhost", "local", "internal", "svc", "cluster.local"} {
		if ascii == suffix || strings.HasSuffix(ascii, "."+suffix) {
			local = true
		}
	}
	return poolHost{name: ascii, port: port, local: local}, nil
}

func parsePoolGateway(raw string) (poolHost, error) {
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return poolHost{}, fmt.Errorf("gateway URL must not contain whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return poolHost{}, fmt.Errorf("gateway URL is not parseable")
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Opaque != "" || u.Host == "" {
		return poolHost{}, fmt.Errorf("gateway URL requires http or https and a host")
	}
	if u.User != nil || strings.ContainsAny(raw, "?#") {
		return poolHost{}, fmt.Errorf("gateway URL must not contain userinfo, query or fragment")
	}
	h, err := parsePoolHost(u.Host)
	if err != nil {
		return poolHost{}, fmt.Errorf("gateway URL: %w", err)
	}
	if h.port == "" {
		h.port = "443"
		if scheme == "http" {
			h.port = "80"
		}
	}
	return h, nil
}

func (p RunnerPool) ValidateHosts() error {
	if p.GatewayBaseURL != "" {
		if _, err := parsePoolGateway(p.GatewayBaseURL); err != nil {
			return err
		}
	}
	for i, host := range p.ForgeHosts {
		if _, err := parsePoolHost(host); err != nil {
			return fmt.Errorf("forge_hosts[%d]: %w", i, err)
		}
	}
	return nil
}

// HostWarnings derives its answer on each read. It is not durable policy and
// never resolves or silently rewrites a name the operator supplied.
func (r *RunnerPools) HostWarnings() []PoolHostWarning {
	var out []PoolHostWarning
	if r == nil {
		return out
	}
	for _, p := range r.Pools {
		add := func(field string, h poolHost, err error) {
			if err == nil && h.local {
				out = append(out, PoolHostWarning{Code: PoolGatewayProjectionWarningCode, Pool: p.Name, Field: field, Message: "private or local host: validate the cluster egress policy; this registry projection does not enforce a run boundary"})
			}
		}
		if p.GatewayBaseURL != "" {
			h, err := parsePoolGateway(p.GatewayBaseURL)
			add("gateway_base_url", h, err)
		}
		for i, host := range p.ForgeHosts {
			h, err := parsePoolHost(host)
			add(fmt.Sprintf("forge_hosts[%d]", i), h, err)
		}
	}
	return out
}

type RunnerPoolsReader interface {
	Get(context.Context) (*RunnerPools, error)
}

// CheckPoolGatewayAtBoot compares the two facts only a pool pod can see:
// its effective gateway env and the registry. It warns once and returns;
// launch-frozen boundaries remain the authority for every run.
func CheckPoolGatewayAtBoot(ctx context.Context, reader RunnerPoolsReader, pool, effectiveURL string, warn func(string, ...any)) {
	if pool == "" || warn == nil {
		return
	}
	say := func(reason string) {
		warn("%s: runner pool %q gateway projection: %s", PoolGatewayProjectionWarningCode, pool, reason)
	}
	if reader == nil {
		say("registry reader unavailable")
		return
	}
	bounded, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	registry, err := reader.Get(bounded)
	if err != nil {
		say(fmt.Sprintf("registry read failed: %v", err))
		return
	}
	var entry *RunnerPool
	if registry != nil {
		for i := range registry.Pools {
			if registry.Pools[i].Name == pool {
				entry = &registry.Pools[i]
				break
			}
		}
	}
	if entry == nil || entry.GatewayBaseURL == "" {
		say("registry gateway projection is missing")
		return
	}
	actual, err := parsePoolGateway(effectiveURL)
	if err != nil {
		say(fmt.Sprintf("effective gateway: %v", err))
		return
	}
	declared, err := parsePoolGateway(entry.GatewayBaseURL)
	if err != nil {
		say(fmt.Sprintf("stored gateway: %v", err))
		return
	}
	if actual.name != declared.name || actual.port != declared.port {
		say(fmt.Sprintf("effective host %s differs from registry host %s", net.JoinHostPort(actual.name, actual.port), net.JoinHostPort(declared.name, declared.port)))
	}
}
