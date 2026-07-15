package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// cloneAllowPrivate reports whether the on-prem escape hatch for internal
// forges is set. Shared by validateRepoTarget's pre-check and the clone-guard
// proxy's connect-time dial so the two layers can never disagree.
func cloneAllowPrivate() bool {
	return os.Getenv("ITERION_RUNNER_CLONE_ALLOW_PRIVATE") == "1"
}

// startCloneGuardProxy starts a loopback CONNECT proxy that gives the runner's
// git subprocesses connect-time SSRF enforcement — the layer validateRepoTarget's
// pre-check structurally cannot provide, because git re-resolves the hostname in
// its own subprocess. The proxy re-resolves the CONNECT target itself through
// the shared SSRF guard and dials ONLY the validated IP (public-unicast in
// strict mode), so a DNS-rebinding answer between the pre-check and git's
// connect hits the same guard again at the moment that matters. Unlike the
// /etc/hosts pin, this works on non-root pods (kubelet-owned hosts file) and
// needs no cluster egress NetworkPolicy.
//
// The policy additionally allowlists the single repo host, so a redirect or
// alternate-URL fetch to any other host is refused outright (defence in depth
// on top of http.followRedirects=false).
//
// strict mirrors validateRepoTarget: on-prem deployments cloning internal
// forges (ITERION_RUNNER_CLONE_ALLOW_PRIVATE=1) keep working — the dial then
// pins the first resolved address without the public-unicast requirement.
//
// Covers http(s) transports only (git ignores HTTPS_PROXY for ssh remotes);
// ssh clones keep the pre-check + pod egress policy as their guard.
func startCloneGuardProxy(repoHost string, strict bool) (endpoint string, shutdown func(), err error) {
	policy, err := netproxy.Compile(netproxy.ModeAllowlist, []string{repoHost})
	if err != nil {
		return "", nil, fmt.Errorf("clone-guard proxy: compile policy for %q: %w", repoHost, err)
	}
	token, err := netproxy.NewToken()
	if err != nil {
		return "", nil, fmt.Errorf("clone-guard proxy: mint token: %w", err)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	proxy, err := netproxy.New(netproxy.Options{
		Policy: policy,
		Token:  token,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				return nil, splitErr
			}
			ip, resolveErr := httpdial.ResolvePublicHost(ctx, host, strict)
			if resolveErr != nil {
				return nil, resolveErr
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("clone-guard proxy: %w", err)
	}
	if err := proxy.Start("127.0.0.1:0"); err != nil {
		return "", nil, fmt.Errorf("clone-guard proxy: %w", err)
	}
	shutdown = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	}
	return proxy.Endpoint("127.0.0.1"), shutdown, nil
}

// cloneGuardEnv renders the environment that routes a git subprocess through
// the clone-guard proxy. Both spellings are set because git consults the
// lowercase forms first and libcurl accepts either; NO_PROXY is cleared so an
// inherited exclusion cannot bypass the guard for the repo host.
func cloneGuardEnv(endpoint string) []string {
	return []string{
		"HTTPS_PROXY=" + endpoint,
		"https_proxy=" + endpoint,
		"HTTP_PROXY=" + endpoint,
		"http_proxy=" + endpoint,
		"NO_PROXY=",
		"no_proxy=",
	}
}
