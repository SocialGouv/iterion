package kubernetes

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The start probe of ADR-162 decision 7: before the first spawn, the
// sandbox start PROVES the untrusted run's network floor. A probe that
// ignores the proxy env dials the roads the per-run policy must have
// closed — the cluster API service, the credential's own host, the
// runner pod's metrics port — and every one of those direct dials must
// FAIL. A non-enforcing CNI (kind's kindnetd) applies the NetworkPolicy
// as a no-op, and only this probe sees it: a dial that connects refuses
// the run, typed, before any workspace code runs.

// clusterAPITarget is the in-cluster API service every pod's network
// namespace can name: a road to the control plane the untrusted policy
// never admits.
const clusterAPITarget = "kubernetes.default.svc:443"

// defaultMetricsPort mirrors pkg/config's MetricsConfig default; the
// runner's own Prometheus listener — a port on the runner pod no
// untrusted policy names.
const defaultMetricsPort = 9090

// networkFloorProbeTargets names every host:port the probe must find
// unreachable: the cluster API service, the credential's own hosts (the
// fork lane's whole allowlist — through the proxy they serve; directly
// they must not), and the runner pod's metrics port.
func networkFloorProbeTargets(info sandbox.RunInfo, runnerIP string) []string {
	targets := []string{clusterAPITarget}
	for _, h := range info.EgressAllowlistHosts {
		if h != "" {
			targets = append(targets, h+":443")
		}
	}
	if runnerIP != "" {
		targets = append(targets, runnerIP+":"+strconv.Itoa(resolveMetricsPort()))
	}
	return targets
}

// resolveMetricsPort reads the runner's Prometheus port from the same
// env pkg/config does, falling back to its default.
func resolveMetricsPort() int {
	if v := os.Getenv("ITERION_METRICS_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p >= 1 && p <= 65535 {
			return p
		}
	}
	return defaultMetricsPort
}

// buildNetworkFloorProbe renders the in-container probe script: POSIX
// sh (the image's shell, not bash), the proxy env UNSET (the probe is
// the client that ignores it), one dial attempt per target through the
// first dial-capable tool the image carries. A dial that connects is
// the verdict; no tool at all is NOPROBE — the floor is unproven, which
// for an untrusted run is the same refusal.
func buildNetworkFloorProbe(targets []string) []string {
	script := `unset HTTP_PROXY HTTPS_PROXY http_proxy https_proxy ALL_PROXY all_proxy NO_PROXY no_proxy
for t in "$@"; do
  h=${t%:*}; p=${t##*:}
  if command -v timeout >/dev/null 2>&1 && command -v bash >/dev/null 2>&1; then
    if timeout 5 bash -c "exec 3<>/dev/tcp/$h/$p" 2>/dev/null; then echo "CONNECTED $t"; exit 1; fi
  elif command -v nc >/dev/null 2>&1; then
    if nc -z -w 5 "$h" "$p" 2>/dev/null; then echo "CONNECTED $t"; exit 1; fi
  else
    echo NOPROBE; exit 2
  fi
done
echo REFUSED
exit 0`
	return append([]string{"sh", "-c", script, "network-floor-probe"}, targets...)
}

// probeNetworkFloor execs the probe in the sibling pod and reads its
// verdict. REFUSED (exit 0) is the floor holding — the start goes on.
// CONNECTED names the road that stayed open; NOPROBE (or an exec that
// never answered) is a floor nothing witnessed. Both refusals are
// typed; the caller cleans the pod up.
func (r *Run) probeNetworkFloor(ctx context.Context, targets []string) error {
	res, err := r.Exec(ctx, buildNetworkFloorProbe(targets), sandbox.ExecOpts{})
	if err != nil {
		return &sandbox.NetworkFloorProbeError{Unproven: true, Detail: fmt.Sprintf("the probe exec failed: %v", err)}
	}
	out := strings.TrimSpace(string(res.Stdout))
	for _, line := range strings.Split(out, "\n") {
		if target, ok := strings.CutPrefix(strings.TrimSpace(line), "CONNECTED "); ok {
			return &sandbox.NetworkFloorProbeError{Target: target}
		}
	}
	if res.ExitCode == 0 && out == "REFUSED" {
		return nil
	}
	detail := out
	if detail == "" {
		detail = fmt.Sprintf("exit %d with no verdict", res.ExitCode)
	}
	return &sandbox.NetworkFloorProbeError{Unproven: true, Detail: detail}
}
