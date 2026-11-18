#!/usr/bin/env bash
# The containerd arm of the codemode posture bench (#3155, ADR-165).
#
# WHY A SECOND ARM. `seccompProfile: RuntimeDefault` names whatever the RUNTIME
# defaults to, so it is a different filter under docker than under containerd.
# codemode-inpod-bench.sh measures the docker one; measuring only that would
# answer for production by assumption. This arm puts the SHIPPED image in a real
# Pod carrying the chart's securityContext, on a `kind` cluster whose nodes run
# containerd — `kind` and `kubectl` are devbox packages of this repo.
#
# WHAT IT STILL DOES NOT SETTLE: the production node's own containerd build. A
# default profile belongs to its build, so this arm reports the version it
# measured and that version is part of the verdict, not a detail.
#
# FOUR THINGS THIS SCRIPT KNOWS THE HARD WAY, each a wasted run otherwise:
#   - `kind` wants to write ~/.kube/config, which is read-only under the agent
#     sandbox: every call passes --kubeconfig into the evidence dir instead.
#   - the API server is published on the host loopback, which that sandbox does
#     not share, so kubectl is driven from INSIDE the node over the docker
#     socket rather than from here.
#   - `docker cp` into a path under one of the node's mounts reports success for
#     a file the node cannot see; manifests go to / .
#   - `kind load docker-image` takes a NAME, so a digest-pinned image is tagged
#     locally first. The digest is what the operator passes and what gets
#     reported; the tag never leaves this script.
set -Eeuo pipefail

say() { printf 'codemode-cluster-bench: %s\n' "$*" >&2; }
die() { say "$*"; exit 2; }

# ONE guard for every setup step, instead of an N+1-th `|| die`. A naked
# command dying under `set -e` used to exit 1 — a red verdict about a
# deployment — from eleven sites; measured with an evidence dir under a file,
# where `mkdir` failed and the member answered 1. Anything that reaches here
# unhandled means nothing was tested, which is this repo's 2.
trap 'rc=$?; say "nothing was tested: a setup step failed at line $LINENO (exit $rc)"; exit 2' ERR

usage() {
	cat >&2 <<'USAGE'
usage: codemode-cluster-bench.sh --image <repo@sha256:…> [--node-image REF] [--keep]

  --image       the runner image, pinned by DIGEST.
  --node-image  the kind node image, pinned by DIGEST. It decides WHICH
                containerd is measured, so leaving it to kind's default means
                measuring whatever that default moved to — this script
                reported containerd 2.1.1 and then 2.2.0 for the same command.
  --keep        leave the cluster up for inspection (otherwise it is deleted).

  ITERION_CI_GATE_EVIDENCE=<dir> keeps the logs and the kubeconfig.
USAGE
	exit 2
}

# The node image is pinned by digest for the reason in the usage text. Bump it
# deliberately, and record the containerd version the new one reports.
NODE_IMAGE="kindest/node:v1.33.1@sha256:050072256b9a903bd914c0b2866828150cb229cea0efe5892e2b644d5dd3b34f"
IMAGE="" KEEP=""
while [ $# -gt 0 ]; do
	case "$1" in
	--image)
		[ $# -ge 2 ] || die "--image needs a value"
		[ -z "$IMAGE" ] || die "--image given twice ('$IMAGE' then '$2'): refusing to guess which image you meant"
		IMAGE=$2
		shift 2
		;;
	--node-image)
		[ $# -ge 2 ] || die "--node-image needs a value"
		NODE_IMAGE=$2
		shift 2
		;;
	--keep) KEEP=1; shift ;;
	*) usage ;;
	esac
done
case "$NODE_IMAGE" in
*@sha256:*) ;;
*) die "--node-image must be pinned by digest, got '$NODE_IMAGE': it decides which containerd is measured" ;;
esac
[ -n "$IMAGE" ] || usage
case "$IMAGE" in
*@sha256:*) ;;
*) echo "codemode-cluster-bench: --image must be pinned by digest, got '$IMAGE'" >&2; exit 2 ;;
esac

cd "$(git rev-parse --show-toplevel)"
EVIDENCE=${ITERION_CI_GATE_EVIDENCE:-$(mktemp -d)}
case "$(cd "$EVIDENCE" 2> /dev/null && pwd -P || echo "$EVIDENCE")" in
"$PWD" | "$PWD"/*) die "ITERION_CI_GATE_EVIDENCE points inside the work tree ($EVIDENCE); the 14 MB bench binary would be left there untracked" ;;
esac
mkdir -p "$EVIDENCE/mount"
KUBECONFIG_PATH="$EVIDENCE/kubeconfig"
CLUSTER=codemode-posture
NODE="${CLUSTER}-control-plane"
# kubectl runs inside the node: see the header.
kube() { docker exec "$NODE" kubectl --kubeconfig /etc/kubernetes/admin.conf "$@"; }

cleanup() {
	# The tag is this script's own, created to feed `kind load docker-image`,
	# which takes a NAME and not a digest. Leaving it behind on the host was
	# litter from every run.
	docker rmi codemode-posture-image:local > /dev/null 2>&1 || true
	if [ -n "$KEEP" ]; then
		say "cluster kept: kind delete cluster --name $CLUSTER"
		return
	fi
	kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG_PATH" > /dev/null 2>&1 || true
}
trap cleanup EXIT

say "building the bench, CGO_ENABLED=0 — a cgo-linked child cannot confine its own threads"
CGO_ENABLED=0 go build -o "$EVIDENCE/mount/inpodbench" ./scripts/ci/inpodbench \
	|| die "the bench could not be built, so nothing was tested"
# `go build` honours the umask; a 0700 binary is unexecutable by the pod's uid.
chmod 0755 "$EVIDENCE/mount/inpodbench"

cat > "$EVIDENCE/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $CLUSTER
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: $EVIDENCE/mount
        containerPath: /bench
        readOnly: true
EOF

say "creating the cluster"
kind create cluster --image "$NODE_IMAGE" --config "$EVIDENCE/kind.yaml" \
	--kubeconfig "$KUBECONFIG_PATH" > "$EVIDENCE/kind-create.log" 2>&1 \
	|| { say "kind could not create a cluster. Refusing is not a verdict: nothing was tested. $EVIDENCE/kind-create.log"; exit 2; }

RUNTIME=$(docker exec "$NODE" containerd --version 2>&1 | head -1)
say "the runtime under test: $RUNTIME (node image $NODE_IMAGE)"

say "loading $IMAGE into the cluster"
docker tag "$IMAGE" codemode-posture-image:local || die "the image could not be tagged for kind"
kind load docker-image codemode-posture-image:local --name "$CLUSTER" > "$EVIDENCE/kind-load.log" 2>&1 \
	|| die "the image could not be loaded into the cluster: $EVIDENCE/kind-load.log"

# The kubelet reads a Localhost profile relative to its own seccomp root.
docker exec "$NODE" sh -c 'mkdir -p /var/lib/kubelet/seccomp' > /dev/null
probes=$("$EVIDENCE/mount/inpodbench" -print-probe-set) \
	|| die "the bench would not print its probe set, so the mutant profiles cannot be built from it"
DENIED_PROBES=""
for probe in $probes; do
	DENIED_PROBES="$DENIED_PROBES{\"names\":[\"$probe\"],\"action\":\"SCMP_ACT_ERRNO\",\"errnoRet\":1},"
done
# printf INSIDE the node, not a heredoc into `docker exec`: without -i that
# exec forwards no stdin, so the heredoc went nowhere and the kubelet refused
# an empty profile ("unexpected end of JSON input") for ten minutes.
for pair in eperm:1 enosys:38; do
	name=${pair%%:*} errno=${pair##*:}
	# Written HERE and copied in. Building it inside `docker exec sh -c` meant
	# counting backslashes through four quoting layers, and the kubelet refused
	# the result ("invalid character '\\'") — a profile this script could not
	# write is not a mutant, it is ten minutes of waiting.
	printf '{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[%s{"names":["landlock_create_ruleset"],"action":"SCMP_ACT_ERRNO","errnoRet":%s}]}\n' \
		"$DENIED_PROBES" "$errno" > "$EVIDENCE/deny-landlock-$name.json"
	docker cp "$EVIDENCE/deny-landlock-$name.json" "$NODE:/var/lib/kubelet/seccomp/deny-landlock-$name.json" > /dev/null \
		|| die "the $name seccomp profile could not be copied to the node"
	# Read it back THROUGH a parser, not with grep: the failure that cost a
	# whole wait was a profile that contained the right words and was invalid
	# JSON. With jq and not python3 — jq is a devbox package, python3 here is a
	# host pyenv shim, and this repo's rule is that tooling comes from devbox.
	docker exec "$NODE" sh -c "cat /var/lib/kubelet/seccomp/deny-landlock-$name.json" \
		| jq -e '[.syscalls[].names[]] | index("landlock_create_ruleset")' > /dev/null \
		|| die "the $name seccomp profile on the node is not valid JSON naming landlock_create_ruleset"
done

# One pod per arm. The pod securityContext is charts/iterion/values.yaml's
# `securityContext`, the container's is its `containerSecurityContext`, and the
# resources are values-prod.yaml's runner limits. `seccomp` selects the arm and
# is the ONLY field that differs between them.
emit_pod() {
	local name=$1 seccomp=$2 cmd=$3 args=$4
	cat <<EOF
---
apiVersion: v1
kind: Pod
metadata: {name: $name}
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 10001
    runAsGroup: 10001
    fsGroup: 10001
    seccompProfile: $seccomp
  containers:
    - name: c
      image: codemode-posture-image:local
      imagePullPolicy: Never
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: [ALL]}
        readOnlyRootFilesystem: false
      command: $cmd
      args: $args
      env:
        - {name: TMPDIR, value: /tmp}
      resources:
        requests: {cpu: 500m, memory: 1Gi}
        limits: {cpu: 4000m, memory: 8Gi}
      volumeMounts:
        - {name: bench, mountPath: /bench, readOnly: true}
        - {name: scratch, mountPath: /tmp}
  volumes:
    - name: bench
      hostPath: {path: /bench, type: Directory}
    - name: scratch
      emptyDir: {}
EOF
}

# The $(id -u) is for the CONTAINER's shell, not this one: single quotes are the
# point, not an oversight.
# shellcheck disable=SC2016
POSTURE='["sh","-c","grep -E \"^(Seccomp|Seccomp_filters|NoNewPrivs|Cap(Eff|Bnd|Inh|Amb|Prm)):\" /proc/self/status; echo uid=$(id -u) gid=$(id -g)"]'
BENCH='["/bench/inpodbench"]'
BENCH_ARGS='["-binary","/usr/local/bin/iterion","-require-uid","10001"]'
{
	emit_pod bench-runtimedefault '{type: RuntimeDefault}' "$BENCH" "$BENCH_ARGS"
	emit_pod posture-runtimedefault '{type: RuntimeDefault}' "$POSTURE" '[]'
	emit_pod posture-unconfined '{type: Unconfined}' "$POSTURE" '[]'
	emit_pod mutant-eperm '{type: Localhost, localhostProfile: deny-landlock-eperm.json}' "$BENCH" "$BENCH_ARGS"
	emit_pod mutant-enosys '{type: Localhost, localhostProfile: deny-landlock-enosys.json}' "$BENCH" "$BENCH_ARGS"
} > "$EVIDENCE/pods.yaml"

# NOT /tmp: see the header.
docker cp "$EVIDENCE/pods.yaml" "$NODE:/pods.yaml" > /dev/null \
	|| die "the manifests could not be copied to the node"
kube apply -f /pods.yaml > "$EVIDENCE/apply.log" 2>&1 \
	|| die "the manifests were not applied, so a half-applied cluster proves nothing: $EVIDENCE/apply.log"

PODS="bench-runtimedefault posture-runtimedefault posture-unconfined mutant-eperm mutant-enosys"
say "waiting for every pod to terminate"
deadline=$((SECONDS + 600))
while [ "$SECONDS" -lt "$deadline" ]; do
	pending=0
	for p in $PODS; do
		case "$(kube get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null)" in
		Succeeded | Failed) continue ;;
		esac
		# A container the runtime cannot CREATE never reaches a terminal phase
		# under restartPolicy: Never, so waiting for one burns the whole
		# window. Name it instead of outliving it.
		case "$(kube get pod "$p" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null)" in
		CreateContainerError | CreateContainerConfigError | InvalidImageName | ImagePullBackOff | ErrImagePull | ErrImageNeverPull)
			say "FAIL $p cannot start: $(kube get pod "$p" -o jsonpath='{.status.containerStatuses[0].state.waiting.message}' 2>/dev/null)"
			rc_startup=1
			continue
			;;
		esac
		pending=$((pending + 1))
	done
	[ -n "${rc_startup:-}" ] && break
	[ "$pending" -eq 0 ] && break
	sleep 5
done
if [ -n "${rc_startup:-}" ] || [ "${pending:-1}" -ne 0 ]; then
	# Refusing is not a verdict, and a refusal that keeps nothing cannot be
	# diagnosed: the sibling gate member dumps its container's log on every
	# way out, and so does this.
	for p in $PODS; do
		{
			echo "### $p"
			kube get pod "$p" -o wide 2>&1
			kube describe pod "$p" 2>&1 | tail -25
			echo "--- logs:"
			kube logs "$p" 2>&1 | tail -40
		} >> "$EVIDENCE/timeout-diagnosis.txt"
	done
	say "pods did not terminate in 600s. Nothing is concluded. $EVIDENCE/timeout-diagnosis.txt"
	exit 2
fi

for p in $PODS; do
	# Both reads are guarded: an unreadable log fails every posture grep below,
	# and an unreadable exit code became "reported no exit code" — each a red
	# verdict about a runtime nobody had judged.
	kube logs "$p" > "$EVIDENCE/$p.log" 2>&1 \
		|| die "the logs of $p could not be read, so nothing can be judged: $EVIDENCE/$p.log"
	code=$(kube get pod "$p" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}') \
		|| die "the exit code of $p could not be read, so nothing can be judged"
	printf '%s\n' "$code" > "$EVIDENCE/$p.exit"
done
exit_of() { cat "$EVIDENCE/$1.exit"; }

echo "--- posture, RuntimeDefault"; cat "$EVIDENCE/posture-runtimedefault.log"
echo "--- posture, Unconfined (the twin that proves the filter moves)"; cat "$EVIDENCE/posture-unconfined.log"
echo "--- the bench, RuntimeDefault"; cat "$EVIDENCE/bench-runtimedefault.log"

rc=0
# The filter must be ON in the measured arm and OFF in the twin. Without this,
# a silently unconfined container passes for a confined one and every leg below
# it is decoration.
grep -q '^Seccomp:[[:space:]]*2' "$EVIDENCE/posture-runtimedefault.log" \
	|| { say "FAIL the RuntimeDefault arm carried NO seccomp filter, so it measured an unconfined container"; rc=1; }
grep -q '^Seccomp:[[:space:]]*0' "$EVIDENCE/posture-unconfined.log" \
	|| { say "FAIL the Unconfined twin also reports a filter, so 'Seccomp: 2' above proves nothing"; rc=1; }
grep -q '^uid=10001 ' "$EVIDENCE/posture-runtimedefault.log" \
	|| { say "FAIL the arm did not run as uid 10001, so it is not the runner's posture"; rc=1; }
grep -q '^NoNewPrivs:[[:space:]]*1' "$EVIDENCE/posture-unconfined.log" \
	|| { say "FAIL the Unconfined twin lost no_new_privs too, so it varies two things and cannot attribute either"; rc=1; }

# The bench's exit codes are a contract, and this arm mapped NONE of them: an
# exit 4 ("this pod is not the posture asked about") was announced as
# containerd's RuntimeDefault failing to confine a child.
case "$(exit_of bench-runtimedefault)" in
0) ;;
2) say "nothing was tested: the bench pod refused its own arguments"; exit 2 ;;
4) say "nothing was tested: the bench pod is not the posture asked about"; exit 2 ;;
6) say "a leg could not run in this pod, so the deployment was not judged"; exit 2 ;;
7) say "the bench pod ran without checking its posture, so it says nothing about this cluster"; exit 2 ;;
"") say "the bench pod reported no exit code at all"; rc=1 ;;
*) say "FAIL the bench did not hold under containerd's RuntimeDefault (exit $(exit_of bench-runtimedefault))"; rc=1 ;;
esac
grep -qE '^SELF .*sha256:[0-9a-f]{64}' "$EVIDENCE/bench-runtimedefault.log" \
	|| { say "FAIL the bench printed no SELF line carrying a digest, so this log cannot be attributed to a binary"; rc=1; }
# The profile's IDENTITY — BOTH halves of it. The kubelet merges the
# CONTAINER-level seccompProfile OVER the pod-level one, and reading only the
# pod-level field was a tautology twice over: it is the field this script
# wrote, and it is not the field that decides. Measured on a real cluster: a
# pod with pod-level RuntimeDefault and container-level Localhost(permissive)
# answered RuntimeDefault to that read while keyctl, bpf and perf_event_open
# reached the kernel; with container-level Unconfined it answered
# RuntimeDefault while running Seccomp: 0.
#
# Not hypothetical as a deployment: charts/iterion/values.yaml ships
# `runner.containerSecurityContext: {}`, and a seccompProfile placed there
# changes what the kubelet applies.
# ONE read for both halves of the merge, and it must be NON-EMPTY and EQUAL.
# The first version matched a glob with "" among the accepted values, so "the
# field could not be read" and "the field is absent, hence identical to the
# pod's" were the same answer: measured against a pod that does not exist, the
# pod-level half failed closed and the container-level half PASSED. A read that
# cannot fail is not an identity check.
#
# The sentinel is what makes absence legible: jsonpath prints nothing for a
# missing container-level field, so the template supplies "inherited" and an
# empty OUTPUT can then only mean the read itself failed.
merged=$(kube get pod bench-runtimedefault -o jsonpath='pod={.spec.securityContext.seccompProfile.type}{range .spec.containers[*]} container={.securityContext.seccompProfile.type}{end}' 2>/dev/null)
case "$merged" in
"") say "FAIL the measured pod's seccompProfile could not be read at all, so nothing was identified"; rc=1 ;;
"pod=RuntimeDefault container=") ;;
"pod=RuntimeDefault container=RuntimeDefault") ;;
*) say "FAIL the kubelet applied something other than RuntimeDefault ($merged): it merges the CONTAINER-level field OVER the pod's"; rc=1 ;;
esac
# The in-pod kernel answer is the only witness of what the kubelet actually
# installed, and the bench carries it: its fingerprint probe is what makes
# this pair more than two names.
grep -q 'VERDICT: 7/7 legs held' "$EVIDENCE/bench-runtimedefault.log" \
	|| { say "FAIL the bench pod did not hold, so nothing corroborates the profile the kubelet installed"; rc=1; }

# Exit codes are a CONTRACT (scripts/ci/inpodbench/main.go): 3 is "the child
# could not confine itself", and a mutant is credited ONLY for that. Accepting
# any non-zero let an unexecutable binary, a missing image and a flaky leg all
# pass for proof that this arm can see a confinement failure.
for m in eperm enosys; do
	case "$(exit_of "mutant-$m")" in
	3) say "mutant $m was refused as it must be (exit 3)" ;;
	0)
		say "FAIL mutant $m PASSED, which means this arm cannot see the failure it exists for"
		rc=1
		;;
	*)
		say "FAIL mutant $m ended with $(exit_of "mutant-$m"), not the posture-refused code 3: it broke for some OTHER reason and proves nothing"
		rc=1
		;;
	esac
done

say "runtime measured: $RUNTIME"
[ "$rc" -eq 0 ] || { say "the containerd arm failed. Evidence: $EVIDENCE"; exit "$rc"; }
say "the containerd arm holds, mutants included. Evidence: $EVIDENCE"
