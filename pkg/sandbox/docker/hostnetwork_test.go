package docker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The host-gateway alias must be injected whenever a per-run MCP
// endpoint will be advertised (HostGatewayAlias), NOT only when the
// egress proxy runs: under the default `network: open` no proxy is
// started, and without the alias a Linux container cannot resolve
// host.docker.internal — the advertised board/ask-user MCP endpoints
// are then unreachable and claude-code fails tool registration at
// session start (both are AlwaysLoad servers).
func TestHostNetworkArgs(t *testing.T) {
	const (
		noProxyEnv   = "NO_PROXY=localhost,127.0.0.1,0.0.0.0,host.docker.internal"
		noProxyLower = "no_proxy=localhost,127.0.0.1,0.0.0.0,host.docker.internal"
		aliasArg     = "host.docker.internal:host-gateway"
	)
	cases := []struct {
		name string
		info sandbox.RunInfo
		want []string
	}{
		{
			name: "neither proxy nor MCP listener",
			info: sandbox.RunInfo{},
			want: nil,
		},
		{
			name: "MCP listener planned, no proxy (network: open default)",
			info: sandbox.RunInfo{HostGatewayAlias: true},
			want: []string{
				"--env", noProxyEnv,
				"--env", noProxyLower,
				"--add-host", aliasArg,
			},
		},
		{
			name: "proxy only",
			info: sandbox.RunInfo{ProxyEndpoint: "http://host.docker.internal:9000"},
			want: []string{
				"--env", "HTTPS_PROXY=http://host.docker.internal:9000",
				"--env", "HTTP_PROXY=http://host.docker.internal:9000",
				"--env", "https_proxy=http://host.docker.internal:9000",
				"--env", "http_proxy=http://host.docker.internal:9000",
				"--env", noProxyEnv,
				"--env", noProxyLower,
				"--add-host", aliasArg,
			},
		},
		{
			name: "proxy and MCP listener (no duplicate alias)",
			info: sandbox.RunInfo{ProxyEndpoint: "http://host.docker.internal:9000", HostGatewayAlias: true},
			want: []string{
				"--env", "HTTPS_PROXY=http://host.docker.internal:9000",
				"--env", "HTTP_PROXY=http://host.docker.internal:9000",
				"--env", "https_proxy=http://host.docker.internal:9000",
				"--env", "http_proxy=http://host.docker.internal:9000",
				"--env", noProxyEnv,
				"--env", noProxyLower,
				"--add-host", aliasArg,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostNetworkArgs(tc.info, nil); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("hostNetworkArgs(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// The workload's own NO_PROXY is the operator's: the driver adds the
// sandbox's loopback and host.docker.internal to it instead of replacing it
// — an inherited corporate proxy's exceptions stay exceptions — and both
// spellings end up with the union of what the spec carried under either.
func TestHostNetworkArgsKeepsTheSpecsNoProxyEntries(t *testing.T) {
	args := hostNetworkArgs(
		sandbox.RunInfo{ProxyEndpoint: "http://host.docker.internal:9000"},
		// "localhost" is in both: an entry is kept once, where the operator
		// put it.
		map[string]string{"NO_PROXY": "corp.internal, 10.0.0.0/8, localhost", "no_proxy": "git.corp"},
	)
	const want = "corp.internal,10.0.0.0/8,localhost,git.corp,127.0.0.1,0.0.0.0,host.docker.internal"
	seen := 0
	for i, a := range args {
		if a != "--env" || i+1 >= len(args) {
			continue
		}
		for _, k := range []string{"NO_PROXY=", "no_proxy="} {
			if strings.HasPrefix(args[i+1], k) {
				seen++
				if got := strings.TrimPrefix(args[i+1], k); got != want {
					t.Errorf("%s%s, want %s%s", k, got, k, want)
				}
			}
		}
	}
	if seen != 2 {
		t.Errorf("the driver set %d no-proxy variable(s), want both spellings", seen)
	}
}
