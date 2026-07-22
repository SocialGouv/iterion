package docker

import (
	"reflect"
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
		noProxyEnv = "NO_PROXY=localhost,127.0.0.1,host.docker.internal"
		aliasArg   = "host.docker.internal:host-gateway"
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
				"--add-host", aliasArg,
			},
		},
		{
			name: "proxy only",
			info: sandbox.RunInfo{ProxyEndpoint: "http://host.docker.internal:9000"},
			want: []string{
				"--env", "HTTPS_PROXY=http://host.docker.internal:9000",
				"--env", "HTTP_PROXY=http://host.docker.internal:9000",
				"--env", noProxyEnv,
				"--add-host", aliasArg,
			},
		},
		{
			name: "proxy and MCP listener (no duplicate alias)",
			info: sandbox.RunInfo{ProxyEndpoint: "http://host.docker.internal:9000", HostGatewayAlias: true},
			want: []string{
				"--env", "HTTPS_PROXY=http://host.docker.internal:9000",
				"--env", "HTTP_PROXY=http://host.docker.internal:9000",
				"--env", noProxyEnv,
				"--add-host", aliasArg,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostNetworkArgs(tc.info); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("hostNetworkArgs(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}
