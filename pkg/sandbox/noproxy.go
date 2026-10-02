package sandbox

import "strings"

// LoopbackNoProxy is the NO_PROXY value a driver sets for the sandbox's own
// loopback: a server the workload binds there is reached directly, never on
// the egress proxy's host. It names no IPv6 loopback: clients read entries as
// patterns, and every spelling of it breaks a mainstream one — a bare ::1 is
// reduced to the host "1" by Ruby's Net::HTTP (every IPv4 address ending in
// .1 would go around the proxy) and matched as a suffix by Python's
// requests, while [::1] and ::1/128 make httpx refuse to build a client.
const LoopbackNoProxy = "localhost,127.0.0.1,0.0.0.0"

// MergeNoProxy returns the sandbox's own loopback entries (and any extra a
// driver adds, such as docker's host.docker.internal) joined to the entries
// the spec already carried, in order and without repeats. The workload's
// NO_PROXY is the operator's: an inherited corporate proxy's exceptions stay
// exceptions, and the loopback is added rather than substituted for them.
//
// It is the docker driver's posture, where nothing enforces egress and the
// spec's env is the operator's own. The kubernetes driver REPLACES instead:
// a pod's egress is locked by a synthesized NetworkPolicy, and an entry kept
// from the spec would carve a hole in the allowlist it exists to enforce.
func MergeNoProxy(existing string, extra ...string) string {
	var out []string
	seen := map[string]bool{}
	add := func(list string) {
		for _, entry := range strings.Split(list, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" || seen[entry] {
				continue
			}
			seen[entry] = true
			out = append(out, entry)
		}
	}
	add(existing)
	add(LoopbackNoProxy)
	for _, e := range extra {
		add(e)
	}
	return strings.Join(out, ",")
}
