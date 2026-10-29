#!/usr/bin/env bash
# Why `cloud-e2e` cannot run on the arc-runners scale set (#981) — measured on
# a real ARC pod instead of hypothesised.
#
# Three of the nine Tests jobs used to be GitHub-hosted. Two moved on
# 2026-09-29 (#1955): `race` once the scale set's image shipped gcc, and
# `mongo-conformance` once it started its replica set with `docker run` on
# 127.0.0.1 behind the dind sidecar's startupProbe. `cloud-e2e` stayed, and
# the reason is the one thing nobody established: "kind comes up, kubectl
# cannot authenticate to it". The first hypothesis — the image's user is not
# in the docker group — is refuted by the tree itself: `nats-conformance` and
# `mongo-conformance` reach the daemon from that same image.
#
# This probe captures the facts that separate the live hypotheses, and names
# which one the capture supports. It is driven by
# .github/workflows/arc-kind-probe.yml, which brings a kind cluster up through
# the SAME pinned action `cloud-e2e` uses, so the measured path is the failing
# path and not a re-implementation of it.
#
# The hypotheses, and what distinguishes them:
#
#   * The daemon is unreachable. Refuted by two sibling jobs, measured here
#     anyway so the refutation is a fact rather than a reading.
#   * kind writes an endpoint the runner cannot reach. kind reads the node
#     container's port mapping and writes `<HostIp>:<HostPort>`
#     (pkg/cluster/internal/providers/docker/provider.go, GetAPIServerEndpoint),
#     so with `apiServerAddress` at its 127.0.0.1 default the kubeconfig names
#     a loopback port published by the dind daemon. The daemon is a sidecar in
#     the same pod, so that publication lands in the pod's own network
#     namespace — which is exactly why publishing on 127.0.0.1 works for
#     mongo-conformance. Measured here against the node's in-network address.
#   * An egress proxy takes the API server with it. `kubectl` is a Go client:
#     it honours HTTPS_PROXY unless NO_PROXY excludes the host. The docker CLI
#     talks to a unix socket and the Mongo driver does not read those
#     variables, so a proxy that swallows the API server would leave every
#     sibling job green and only break the one that speaks HTTPS to a
#     published port. This is the hypothesis the capture exists to settle.
#
# Modes:
#   arc-kind-probe.sh                  capture on a runner, then classify
#   arc-kind-probe.sh --classify FILE  classify a captured facts file
#
# The classification is the half a harness can exercise, and
# internal/ciguard/arc_kind_probe_test.go holds every rule of it to a fixture.
# The capture half is the gesture: it needs a real pod and is read in the run.
#
# Proxy URLs can carry credentials, so only their presence is recorded —
# never their value. NO_PROXY carries no secret and is recorded in full: it is
# the fact the proxied-apiserver verdict turns on.
set -euo pipefail

usage() {
	echo "usage: arc-kind-probe.sh [--classify <facts file>]" >&2
	exit 2
}

# ----------------------------------------------------------------- classify

classify() {
	local facts="$1"
	[ -f "$facts" ] || { echo "arc-kind-probe: no such facts file: $facts" >&2; exit 2; }

	local docker_reachable kind_setup server server_direct server_via_env node_direct kubectl_ok no_proxy
	docker_reachable="$(fact "$facts" docker_reachable)"
	kind_setup="$(fact "$facts" kind_setup_outcome)"
	server="$(fact "$facts" server)"
	server_direct="$(fact "$facts" server_direct)"
	server_via_env="$(fact "$facts" server_via_env)"
	node_direct="$(fact "$facts" node_ip_direct)"
	kubectl_ok="$(fact "$facts" kubectl_cluster_info)"
	no_proxy="$(fact "$facts" no_proxy)"

	# A fact the capture did not record is not a verdict: every rule below
	# reads one, and "-" would otherwise be answered as docker-unreachable —
	# a diagnosis drawn from a truncated capture.
	# These two the capture always records, whatever it found; the others
	# carry "-" legitimately (no cluster, no endpoint, nothing to reach).
	for required in docker_reachable kubectl_cluster_info; do
		if [ "$(fact "$facts" "$required")" = "-" ]; then
			verdict capture-incomplete "the capture recorded no '$required', so these facts cannot support any verdict — read the run's log before reading this"
			return
		fi
	done

	# The order is the decision: each rule names a cause the rules after it
	# would mistake for theirs.
	if [ "$docker_reachable" != "yes" ]; then
		verdict docker-unreachable "the runner cannot reach the docker daemon at all, so nothing about kind is established here — and the sibling jobs that do reach it say this is not the usual state"
	elif [ "$kind_setup" != "success" ] || [ "$server" = "-" ]; then
		verdict cluster-not-created "kind did not bring a cluster up (setup outcome '$kind_setup', kubeconfig server '$server'), so the failure is in the cluster's creation, not in reaching it"
	elif answered "$server_direct" && ! answered "$server_via_env"; then
		verdict proxied-apiserver "the API server at $server answers when the proxy environment is bypassed ($server_direct) and does not otherwise ($server_via_env): an egress proxy is taking a loopback address. NO_PROXY is '$no_proxy' — it must cover the kind endpoint"
	elif ! answered "$server_direct" && answered "$node_direct"; then
		verdict kubeconfig-endpoint-unreachable "the endpoint kind wrote ($server) refuses the connection while the node's in-network address answers ($node_direct): the published port is not reachable from this container"
	elif ! answered "$server_direct"; then
		verdict apiserver-unreachable "neither the endpoint kind wrote ($server) nor the node's in-network address answers: the cluster is up but nothing in this container can reach its API server"
	elif [ "$kubectl_ok" != "ok" ]; then
		verdict kubectl-refused "the API server at $server answers the raw request ($server_direct) but kubectl could not talk to it: the failure is in the credentials or the TLS material the kubeconfig carries, not in reachability"
	else
		verdict usable "kind came up and kubectl reached it from the runner: the scale set can host this job, and what kept cloud-e2e GitHub-hosted is no longer reproducible"
	fi
}

# fact <file> <key> — the value of a key, "-" when absent. One key per line,
# first occurrence wins, so a capture cannot be contradicted by a later line.
fact() {
	local value
	value="$(awk -F= -v k="$2" '$1 == k { sub(/^[^=]*=/, ""); print; exit }' "$1")"
	[ -n "$value" ] || value="-"
	printf '%s' "$value"
}

# answered <status> — an HTTP status is an answer, however unauthorised; curl
# writes 000 when it never got one.
answered() {
	case "$1" in
		"" | - | 000 | refused) return 1 ;;
		*) return 0 ;;
	esac
}

verdict() {
	printf 'verdict=%s\nbecause=%s\n' "$1" "$2"
	[ "$1" = "usable" ]
}

# ------------------------------------------------------------------ capture

# status_of <url> <direct|via-env> — the HTTP status the API server answers,
# or 000. TLS is deliberately unverified: this asks whether something answers,
# not whether its certificate is trusted.
status_of() {
	local url="$1" how="$2" args=(-sk -o /dev/null -w '%{http_code}' --max-time 10)
	[ "$how" = "direct" ] && args+=(--noproxy '*')
	curl "${args[@]}" "$url" 2>/dev/null || printf '000'
}

capture() {
	local facts="$1"
	: > "$facts"

	if docker version --format '{{.Server.Version}}' > /dev/null 2>&1; then
		echo "docker_reachable=yes" >> "$facts"
		echo "docker_server=$(docker version --format '{{.Server.Version}}' 2>/dev/null)" >> "$facts"
	else
		echo "docker_reachable=no" >> "$facts"
	fi
	echo "kind_setup_outcome=${KIND_SETUP_OUTCOME:-unknown}" >> "$facts"

	# Presence only: a proxy URL can carry credentials.
	for name in HTTP_PROXY http_proxy HTTPS_PROXY https_proxy; do
		[ -n "${!name:-}" ] && echo "proxy_${name}=set" >> "$facts" || echo "proxy_${name}=unset" >> "$facts"
	done
	echo "no_proxy=${NO_PROXY:-${no_proxy:--}}" >> "$facts"

	local server=- node_ip=-
	if server="$(kubectl config view --minify --output 'jsonpath={.clusters[0].cluster.server}' 2>/dev/null)" && [ -n "$server" ]; then
		echo "server=${server#https://}" >> "$facts"
	else
		server=""
		echo "server=-" >> "$facts"
	fi
	echo "kubeconfig=${KUBECONFIG:-$HOME/.kube/config}" >> "$facts"

	if [ -n "$server" ]; then
		echo "server_direct=$(status_of "$server/version" direct)" >> "$facts"
		echo "server_via_env=$(status_of "$server/version" via-env)" >> "$facts"
	else
		echo "server_direct=-" >> "$facts"
		echo "server_via_env=-" >> "$facts"
	fi

	# The node container's own address inside the docker network: the other
	# end of the published port, and the one the kubeconfig does NOT name.
	# One address, deterministically: a node on two networks would otherwise
	# concatenate into a string nothing can reach, and an unreachable garbage
	# address reads as a verdict. text/template walks a map in key order, so
	# the first field is stable.
	if node_ip="$(docker inspect --format '{{ range .NetworkSettings.Networks }}{{ printf "%s " .IPAddress }}{{ end }}' "${KIND_CLUSTER:-kind}-control-plane" 2>/dev/null | awk '{ print $1 }')" && [ -n "$node_ip" ]; then
		echo "node_ip=$node_ip" >> "$facts"
		echo "node_ip_direct=$(status_of "https://$node_ip:6443/version" direct)" >> "$facts"
	else
		echo "node_ip=-" >> "$facts"
		echo "node_ip_direct=-" >> "$facts"
	fi

	if kubectl cluster-info > "$facts.cluster-info" 2>&1; then
		echo "kubectl_cluster_info=ok" >> "$facts"
	else
		echo "kubectl_cluster_info=failed" >> "$facts"
	fi

	echo "--- facts ---"
	cat "$facts"
	echo "--- kubectl cluster-info ---"
	cat "$facts.cluster-info"
	echo "--- docker ps ---"
	docker ps --format '{{.Names}}  {{.Status}}  {{.Ports}}' 2>&1 || true
	echo "--- verdict ---"
}

# --------------------------------------------------------------------- main

case "${1:-}" in
	--classify)
		[ $# -eq 2 ] || usage
		classify "$2"
		;;
	"")
		work="$(mktemp -d)"
		trap 'rm -rf -- "$work"' EXIT
		capture "$work/facts"
		# The summary is where the operator reads a dispatched probe.
		if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
			{
				echo "## ARC kind probe (#981)"
				echo
				echo '```'
				cat "$work/facts"
				classify "$work/facts" || true
				echo '```'
			} >> "$GITHUB_STEP_SUMMARY"
		fi
		# Exit status IS the headline: 0 when the scale set can host kind.
		classify "$work/facts"
		;;
	*) usage ;;
esac
