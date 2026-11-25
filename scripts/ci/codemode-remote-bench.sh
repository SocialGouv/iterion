#!/usr/bin/env bash
# The codemode posture bench, driven against a cluster this host does not own
# (#3299, ADR-165).
#
# WHAT IT SETTLES that the `kind` arm cannot: a `seccompProfile: RuntimeDefault`
# names whatever the RUNTIME defaults to, and a default profile is a property of
# a containerd BUILD. `codemode-cluster-bench.sh` measures the containerd inside
# the pinned kind node image — 2.1.1 for the image it pins. A production node
# ships its own, so this arm runs the same bench on the cluster that serves
# iterion.cloud and records the version the node it landed on reports.
#
# WHAT IT DELIBERATELY DOES NOT SETTLE — printed as a GAP line in every run:
#   - the two `Localhost` seccomp mutants, the control that proves this bench
#     can SEE a confinement failure. A Localhost profile is read by the kubelet
#     from its own seccomp root on the NODE; installing one remotely means a
#     privileged DaemonSet or root on a production machine. The mutant control
#     therefore stays where it is already green (the kind arm), and this arm
#     says so rather than reporting a five-pod verdict it did not earn.
#   - leg 5 of the bench runs the BENCH's own `Confine` (built from HEAD), not
#     the image's; legs 1-4, 6 and 7 exercise the image's own `iterion`.
#
# TWO POSTURES. `--posture runner` (the default) measures the chart's runner
# pod, the in-pod topology. `--posture sandbox` measures the pod the kubernetes
# sandbox driver creates for a run (#3444) — the posture a sandboxed claw node's
# script host is born in — rendered by the driver's own builder so it cannot
# drift from what a run gets.
#
# THE ONE THING IT VARIES, and refuses to vary: the seccomp profile. The
# measured pod and its Unconfined twin share uid, capabilities, no_new_privs and
# resources, so `Seccomp: 2` vs `Seccomp: 0` attributes to the profile alone. If
# the namespace's admission policy refuses the twin, that control is MISSING and
# this script records it as a gap — it does not silently measure one pod and
# call the profile identified.
#
# EXIT CODES ARE A CONTRACT, borrowed from the other arms because a caller that
# reads "non-zero" as "the bound bit" accepts a broken run as proof:
#   0 green · 1 a red verdict · 2 nothing was tested (setup, admission, RBAC).
set -Eeuo pipefail

say() { printf 'codemode-remote-bench: %s\n' "$*" >&2; }
die() { say "$*"; exit 2; }

# ONE guard for every step this script does not handle by name, as in the
# sibling arms: under `set -e` a naked failure exits with the failing command's
# status — 1, the code this contract reserves for a red verdict — and silently.
# Measured: `--uid` given without a value died on `shift 2` with exit 1 and no
# message. Anything that reaches here unhandled means nothing was tested.
#
# The trap speaks for the MAIN shell only. errtrace hands it to every command
# substitution, where a polled read that fails is handled by its caller — and
# must not print "nothing was tested" while the script carries on.
trap 'rc=$?; [ "$BASHPID" -eq "$$" ] || exit "$rc"; say "nothing was tested: a step failed at line $LINENO (exit $rc)"; exit 2' ERR

usage() {
	cat >&2 <<'USAGE'
usage: codemode-remote-bench.sh --bench-image <ref> --kubeconfig <path> --namespace <ns> [options]

  --bench-image   the image built by codemode-bench-image.sh. A DIGEST unless
                  --pull-policy Never (the kind witness lane), because a tag can
                  move under the verdict.
  --kubeconfig    passed to kubectl BY REFERENCE. This script never reads it.
  --context       kubectl context (default: the kubeconfig's current one).
  --namespace     where the pods go. default / kube-system / kube-public /
                  kube-node-lease are refused: this arm creates pods, and it
                  has no business doing that next to control-plane workloads.
  --create-namespace
                  create the namespace and DELETE IT on exit (it is ours). It is
                  created with `pod-security.kubernetes.io/enforce=privileged`
                  so the Unconfined twin is admissible — the twin is the control
                  that proves the measured arm's seccomp read can distinguish.
  --uid           the uid the image runs as (default 1000, the devbox runner).
  --expect-groups the supplementary groups this deployment really gives, comma
                  separated. A container runtime adds the IMAGE's own groups
                  (`devbox` is in `sudo` and `users`), which the chart does not
                  decide; the bench refuses to measure unless they are declared,
                  and then requires an EXACT match in both directions.
  --pull-policy   IfNotPresent (default) or Never.
  --posture       runner (default) or sandbox. `runner` measures the chart's
                  runner pod, written here by hand because no Go builder
                  produces it. `sandbox` measures the pod the KUBERNETES
                  SANDBOX DRIVER creates for a run (#3444): rendered by the
                  driver's own builder (scripts/ci/sandboxbenchpod), so the
                  posture measured is the one a sandboxed run gets. Build the
                  bench image FROM the deployment's sandbox image.
  --sandbox-user  the sandbox spec's `user` (sandbox posture only). Empty, the
                  default, is what a spec naming only an image resolves to:
                  the image's own USER runs, and --uid must name it. The arm
                  compares the measured pod with the namespace's LIVE sandbox
                  pods and refuses a posture none of them runs.
  --name-prefix   prefix of the pod names (default codemode-posture); the run
                  appends its timestamp.
  --keep          leave the pods behind for inspection.
  --max-wait      seconds to wait for terminal phases (default 600).

  ITERION_CI_GATE_EVIDENCE=<dir> keeps logs, manifests and verdicts.
USAGE
	exit 2
}

BENCH_IMAGE="" KUBECONFIG_PATH="" CONTEXT="" NS="" CREATE_NS="" UID_WANT=1000 EXPECT_GROUPS=""
PULL_POLICY=IfNotPresent KEEP="" MAX_WAIT=600 POSTURE_OF=runner SANDBOX_USER="" NAME_PREFIX=codemode-posture
while [ $# -gt 0 ]; do
	case "$1" in
	--bench-image | --kubeconfig | --context | --namespace | --uid | --expect-groups | --pull-policy | --max-wait | --posture | --sandbox-user | --name-prefix)
		[ $# -ge 2 ] || die "$1 needs a value"
		case "$1" in
		--bench-image) BENCH_IMAGE=$2 ;;
		--kubeconfig) KUBECONFIG_PATH=$2 ;;
		--context) CONTEXT=$2 ;;
		--namespace) NS=$2 ;;
		--uid) UID_WANT=$2 ;;
		--expect-groups) EXPECT_GROUPS=$2 ;;
		--pull-policy) PULL_POLICY=$2 ;;
		--max-wait) MAX_WAIT=$2 ;;
		--posture) POSTURE_OF=$2 ;;
		--sandbox-user) SANDBOX_USER=$2 ;;
		--name-prefix) NAME_PREFIX=$2 ;;
		esac
		shift 2
		;;
	--create-namespace) CREATE_NS=1; shift ;;
	--keep) KEEP=1; shift ;;
	*) usage ;;
	esac
done

[ -n "$BENCH_IMAGE" ] || usage
[ -n "$KUBECONFIG_PATH" ] || die "--kubeconfig is required: this arm never guesses which cluster it is about to create pods in"
[ -n "$NS" ] || usage
case "$UID_WANT" in '' | *[!0-9]*) die "--uid must be a number, got '$UID_WANT'" ;; esac
case "$MAX_WAIT" in '' | *[!0-9]*) die "--max-wait must be a number, got '$MAX_WAIT'" ;; esac
case "$PULL_POLICY" in IfNotPresent | Never) ;; *) die "--pull-policy must be IfNotPresent or Never, got '$PULL_POLICY'" ;; esac
case "$POSTURE_OF" in runner | sandbox) ;; *) die "--posture must be runner or sandbox, got '$POSTURE_OF'" ;; esac
[ -z "$SANDBOX_USER" ] || [ "$POSTURE_OF" = sandbox ] || die "--sandbox-user only means something with --posture sandbox"
# The prefix names pods this script later deletes BY NAME: a DNS label, and
# short enough that the timestamp and the longest suffix still fit in 63.
case "$NAME_PREFIX" in
'' | -* | *[!a-z0-9-]*) die "--name-prefix must be lower-case letters, digits and dashes, got '$NAME_PREFIX'" ;;
esac
[ "${#NAME_PREFIX}" -le 30 ] || die "--name-prefix is longer than 30 characters: '$NAME_PREFIX'"
# Group IDS, not names: the bench pod parses them, and a name it refused there
# came back as the pod's own usage exit — a caller error that used to be read
# as a red verdict about the cluster. The docs introduce the groups by name
# (`sudo`, `users`), so a name is the natural mistake: refuse it here.
case "$EXPECT_GROUPS" in
'') ;;
*[!0-9,]* | ,* | *, | *,,*) die "--expect-groups takes comma-separated numeric group ids (e.g. 27,100,1000), got '$EXPECT_GROUPS'" ;;
esac
case "$BENCH_IMAGE" in
*@sha256:*) ;;
*)
	# The ONE escape: nothing pulls, so nothing can move. Anywhere else a tag is
	# a reference that answers for whatever it points at today.
	[ "$PULL_POLICY" = "Never" ] || die "--bench-image must be pinned by digest, got '$BENCH_IMAGE' (a tag is only accepted with --pull-policy Never)"
	;;
esac
# This arm creates pods. The three namespaces that belong to the control plane,
# plus `default`, are where an accident is somebody else's outage.
case "$NS" in
default | kube-system | kube-public | kube-node-lease)
	die "refusing to create bench pods in '$NS': this arm needs a namespace of its own"
	;;
esac
command -v kubectl > /dev/null || die "kubectl is not on PATH"

cd "$(git rev-parse --show-toplevel)"
EVIDENCE=${ITERION_CI_GATE_EVIDENCE:-$(mktemp -d)}
case "$(cd "$EVIDENCE" 2> /dev/null && pwd -P || echo "$EVIDENCE")" in
"$PWD" | "$PWD"/*) die "ITERION_CI_GATE_EVIDENCE points inside the work tree ($EVIDENCE)" ;;
esac
mkdir -p "$EVIDENCE"

kube() {
	if [ -n "$CONTEXT" ]; then
		kubectl --kubeconfig "$KUBECONFIG_PATH" --context "$CONTEXT" -n "$NS" "$@"
	else
		kubectl --kubeconfig "$KUBECONFIG_PATH" -n "$NS" "$@"
	fi
}
kube_raw() {
	if [ -n "$CONTEXT" ]; then
		kubectl --kubeconfig "$KUBECONFIG_PATH" --context "$CONTEXT" "$@"
	else
		kubectl --kubeconfig "$KUBECONFIG_PATH" "$@"
	fi
}

# A name nobody else can be using, so cleanup removes OUR pods and nothing else:
# this script must never delete a pod it did not create.
PREFIX="$NAME_PREFIX-$(date +%s)"

CREATED_NS=""
cleanup() {
	if [ -z "$KEEP" ]; then
		for p in "$PREFIX-bench" "$PREFIX-posture" "$PREFIX-posture-unconfined"; do
			kube delete pod "$p" --ignore-not-found --wait=false > /dev/null 2>&1 || true
		done
	fi
	if [ -n "$CREATED_NS" ]; then
		# Deleting the namespace deletes every pod in it, so under --keep the
		# namespace stays: removing it would destroy the evidence --keep
		# promises, in the very lane (--create-namespace) the docs lead with.
		if [ -n "$KEEP" ]; then
			say "kept namespace '$CREATED_NS' and its pods; remove them with: kubectl delete namespace $CREATED_NS"
		else
			kube_raw delete namespace "$CREATED_NS" --ignore-not-found --wait=false > /dev/null 2>&1 || true
		fi
	fi
}
trap cleanup EXIT

# --- the namespace -----------------------------------------------------------
if [ -n "$CREATE_NS" ]; then
	kube_raw create namespace "$NS" > "$EVIDENCE/ns-create.log" 2>&1 \
		|| die "the namespace '$NS' could not be created: $EVIDENCE/ns-create.log"
	CREATED_NS=$NS
	# The twin needs `seccompProfile: Unconfined`, which baseline and restricted
	# both forbid. The namespace is OURS and holds only our pods, so it carries
	# the label that makes the control possible — and the label dies with it.
	kube_raw label namespace "$NS" pod-security.kubernetes.io/enforce=privileged \
		--overwrite > "$EVIDENCE/ns-label.log" 2>&1 \
		|| die "the namespace '$NS' could not be labelled: $EVIDENCE/ns-label.log"
	say "created namespace '$NS' (enforce=privileged, deleted on exit)"
else
	# "Does not exist" and "may not be read" are different answers. A
	# namespace-scoped credential — the usual production one — cannot get the
	# Namespace object at all, and reading that refusal as absence stopped such
	# a credential here with a false message. Only NotFound means absent; a
	# Forbidden read goes on, and the first apply names the namespace if it
	# really is missing.
	if ! kube_raw get namespace "$NS" > /dev/null 2> "$EVIDENCE/ns-get.err"; then
		if grep -q '(NotFound)' "$EVIDENCE/ns-get.err"; then
			die "namespace '$NS' does not exist and --create-namespace was not given"
		elif grep -q '(Forbidden)' "$EVIDENCE/ns-get.err"; then
			say "this credential may not read namespace '$NS' itself; going on — the pods' admission will say if it is missing"
		else
			die "namespace '$NS' could not be read: $(tail -1 "$EVIDENCE/ns-get.err")"
		fi
	fi
fi

# --- the pods ----------------------------------------------------------------
# uid, caps, no_new_privs, resources and the image are IDENTICAL between the
# measured pod and its twin; `seccomp` is the only field that differs. The
# resources are values-prod.yaml's runner requests/limits: the bench's memory
# bombs need a cgroup large enough to hold them, which is also why leg 6 refuses
# rather than reports when it cannot.
say "the measured image: $BENCH_IMAGE (uid $UID_WANT, pull policy $PULL_POLICY, posture $POSTURE_OF)"
# A sandbox pod carries activeDeadlineSeconds, counted from the pod's start —
# image pull included. It outlives this script's own wait by a margin, so a
# slow pull or a long --max-wait ends as the wait's "nothing was tested", never
# as a pod the kubelet killed and the verdict read as a red bench.
POD_DEADLINE=$((MAX_WAIT + 120))
if [ "$POSTURE_OF" = sandbox ]; then
	# Built once, from this tree: the pods are the driver's as this commit
	# renders them, which is the driver a release of this commit runs.
	CGO_ENABLED=0 go build -o "$EVIDENCE/sandboxbenchpod" ./scripts/ci/sandboxbenchpod \
		> "$EVIDENCE/sandboxbenchpod.build.log" 2>&1 \
		|| die "the sandbox pod renderer did not build: $EVIDENCE/sandboxbenchpod.build.log"
fi
emit_pod() {
	local name=$1 seccomp=$2 cmd=$3 args=$4 file="$EVIDENCE/$1.yaml"
	if [ "$POSTURE_OF" = sandbox ]; then
		file="$EVIDENCE/$1.json"
		# Small and short by contract: requests place the pod, they are not
		# the posture. The limit still holds the memory leg's bombs (~1.2 GiB);
		# under less the bench refuses that leg instead of misreporting it.
		local renderer_pull=() profile=${seccomp#*type: }
		profile=${profile%\}}
		[ "$PULL_POLICY" = Never ] && renderer_pull=(-pull-policy Never)
		"$EVIDENCE/sandboxbenchpod" -namespace "$NS" -name "$name" -run "$PREFIX" \
			-image "$BENCH_IMAGE" -user "$SANDBOX_USER" -seccomp "$profile" \
			-command "$cmd" -args "$args" \
			-cpu-request 100m -memory-request 256Mi -cpu-limit 2 -memory-limit 2Gi \
			-deadline "$POD_DEADLINE" \
			"${renderer_pull[@]}" > "$file" 2> "$EVIDENCE/$1.render.err" \
			|| die "the sandbox pod '$name' could not be rendered: $(tail -1 "$EVIDENCE/$1.render.err")"
		printf '%s\n' "$file"
		return
	fi
	cat > "$file" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  labels: {app: codemode-posture-bench, run: "$PREFIX"}
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: $UID_WANT
    runAsGroup: $UID_WANT
    fsGroup: $UID_WANT
    seccompProfile: $seccomp
  containers:
    - name: c
      image: $BENCH_IMAGE
      imagePullPolicy: $PULL_POLICY
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
        - {name: scratch, mountPath: /tmp}
  volumes:
    - name: scratch
      emptyDir: {}
EOF
	printf '%s\n' "$file"
}

# The $(id -u) is for the CONTAINER's shell, not this one.
# shellcheck disable=SC2016
POSTURE='["sh","-c","grep -E \"^(Seccomp|Seccomp_filters|NoNewPrivs|Cap(Eff|Bnd|Inh|Amb|Prm)):\" /proc/self/status; echo uid=$(id -u) gid=$(id -g)"]'
BENCH='["/bench/inpodbench"]'
BENCH_ARGS="[\"-binary\",\"/usr/local/bin/iterion\",\"-require-uid\",\"$UID_WANT\""
if [ -n "$EXPECT_GROUPS" ]; then
	BENCH_ARGS="$BENCH_ARGS,\"-expect-groups\",\"$EXPECT_GROUPS\""
fi
BENCH_ARGS="$BENCH_ARGS]"

# Applied ONE AT A TIME, so an admission refusal names the pod it refused
# instead of failing a five-document apply and leaving the caller to guess.
ADMITTED="" REFUSED=""
admit() {
	local name=$1 file=$2
	if kube apply -f "$file" > "$EVIDENCE/$name.apply.log" 2>&1; then
		ADMITTED="$ADMITTED $name"
		return 0
	fi
	REFUSED="$REFUSED $name"
	say "refused by admission: $name ($(tail -1 "$EVIDENCE/$name.apply.log"))"
	return 1
}

# Rendered BEFORE any admission, each in an assignment: a pod that cannot be
# written stops the run here (the ERR trap: nothing was tested) instead of
# reaching `kubectl apply -f ""` and reading as an admission refusal.
POD_BENCH=$(emit_pod "$PREFIX-bench" '{type: RuntimeDefault}' "$BENCH" "$BENCH_ARGS")
POD_POSTURE=$(emit_pod "$PREFIX-posture" '{type: RuntimeDefault}' "$POSTURE" '[]')
POD_TWIN=$(emit_pod "$PREFIX-posture-unconfined" '{type: Unconfined}' "$POSTURE" '[]')
admit "$PREFIX-bench" "$POD_BENCH" \
	|| die "the measured pod was refused, so nothing was tested: $EVIDENCE/$PREFIX-bench.apply.log"
admit "$PREFIX-posture" "$POD_POSTURE" \
	|| die "the posture readback was refused, so the profile cannot be identified: $EVIDENCE/$PREFIX-posture.apply.log"
HAVE_TWIN=0
if admit "$PREFIX-posture-unconfined" "$POD_TWIN"; then
	HAVE_TWIN=1
fi

# --- the posture a live run has ----------------------------------------------
# The renderer is the driver's builder, but its INPUT is ours: the sandbox
# spec's user above all, which the deployment's sandbox default decides and
# this script cannot read. A live run's pod can be read. So the measured pod is
# compared with every sandbox pod running in the namespace — the same fields,
# read back through the same API so both carry its defaults — and the arm only
# measures a posture at least one live run has. The fact travels with its
# count: which posture, how many of the live pods run it.
LIVE_MATCH=""
if [ "$POSTURE_OF" = sandbox ]; then
	SECURITY='{.spec.securityContext}|{.spec.automountServiceAccountToken}|{.spec.containers[0].securityContext}'
	ours=$(kube get pod "$PREFIX-bench" -o jsonpath="$SECURITY" 2> "$EVIDENCE/ours.err") \
		|| die "the measured pod's security context could not be read back: $(tail -1 "$EVIDENCE/ours.err")"
	# RUNNING pods only: a run's pod that Cleanup never deleted lingers as
	# Succeeded or Failed, possibly rendered by an older driver, and a match on
	# it would certify a posture no current run has.
	kube get pods -l iterion.io/component=sandbox-run --field-selector=status.phase=Running -o jsonpath="{range .items[*]}$SECURITY{\"\\n\"}{end}" \
		> "$EVIDENCE/live-postures.raw" 2> "$EVIDENCE/live-postures.err" \
		|| die "the live sandbox pods could not be listed: $(tail -1 "$EVIDENCE/live-postures.err")"
	sort "$EVIDENCE/live-postures.raw" | uniq -c > "$EVIDENCE/live-postures.txt"
	total=$(grep -c . "$EVIDENCE/live-postures.raw" || true)
	if [ "$total" -eq 0 ]; then
		LIVE_MATCH=none
	else
		same=$(grep -cxF -- "$ours" "$EVIDENCE/live-postures.raw" || true)
		[ "$same" -gt 0 ] \
			|| die "the measured pod is not the posture any of the $total live sandbox pod(s) runs, so it would certify a posture no run has. ours: $ours — live: $(tr '\n' ';' < "$EVIDENCE/live-postures.txt") Pass the deployment's sandbox user with --sandbox-user."
		LIVE_MATCH="$same of $total"
		say "the measured posture is the one $same of $total live sandbox pod(s) run"
	fi
fi

# --- wait for terminal phases ------------------------------------------------
say "waiting up to ${MAX_WAIT}s for every admitted pod to terminate"
deadline=$((SECONDS + MAX_WAIT))
pending=0
while [ "$SECONDS" -lt "$deadline" ]; do
	pending=0
	for p in $ADMITTED; do
		phase=$(kube get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null) || phase=""
		case "$phase" in
		Succeeded | Failed) continue ;;
		esac
		# A container the runtime cannot CREATE never reaches a terminal phase
		# under restartPolicy: Never, so waiting for one burns the whole window.
		# ErrImageNeverPull is the --pull-policy Never lane's own signature —
		# the image is not on the node — and without it that lane burned the
		# whole window to end on "did not terminate".
		waiting=$(kube get pod "$p" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null) || waiting=""
		case "$waiting" in
		CreateContainerError | CreateContainerConfigError | InvalidImageName | ImagePullBackOff | ErrImagePull | ErrImageNeverPull)
			why=$(kube get pod "$p" -o jsonpath='{.status.containerStatuses[0].state.waiting.message}' 2>/dev/null) || why="(unreadable)"
			say "nothing was tested: $p cannot start ($waiting): $why"
			exit 2
			;;
		esac
		pending=$((pending + 1))
	done
	[ "$pending" -eq 0 ] && break
	sleep 5
done
[ "$pending" -eq 0 ] || { say "the pods did not terminate within ${MAX_WAIT}s; refusing to judge a run that is still going. Evidence: $EVIDENCE"; exit 2; }

for p in $ADMITTED; do
	# A pod the kubelet stopped at its deadline did not finish what it was
	# measuring: its exit code is the kill's, not a verdict.
	reason=$(kube get pod "$p" -o jsonpath='{.status.reason}' 2>/dev/null) || reason=""
	[ "$reason" != DeadlineExceeded ] \
		|| die "nothing was tested: $p was stopped at its activeDeadlineSeconds ($POD_DEADLINE s) before it finished"
	# Unreadable logs are "nothing was tested", never a verdict: kubectl's own
	# error text would land in the log and fail every posture grep below —
	# a red about the cluster when the credential lacks `pods/log`, or when the
	# kubelet refused the pod before its container started.
	kube logs "$p" > "$EVIDENCE/$p.log" 2>&1 \
		|| die "the logs of $p could not be read, so nothing can be judged: $EVIDENCE/$p.log"
	code=$(kube get pod "$p" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}') \
		|| die "the exit code of $p could not be read, so nothing can be judged"
	printf '%s\n' "$code" > "$EVIDENCE/$p.exit"
done
exit_of() { cat "$EVIDENCE/$1.exit"; }

echo "--- posture, RuntimeDefault"; cat "$EVIDENCE/$PREFIX-posture.log"
if [ "$HAVE_TWIN" -eq 1 ]; then
	echo "--- posture, Unconfined (the twin that proves the filter moves)"; cat "$EVIDENCE/$PREFIX-posture-unconfined.log"
fi
echo "--- the bench, RuntimeDefault"; cat "$EVIDENCE/$PREFIX-bench.log"

# --- the runtime this arm exists to name -------------------------------------
# Read from the node the MEASURED pod actually landed on, not from the fleet:
# nodes in one cluster routinely differ after a rolling upgrade, and a version
# read off some other node would attribute the verdict to a build that never ran
# it. Every pod must land on a node reporting the same runtime — otherwise the
# twin and the measured pod are not the same filter, and the comparison is void.
# Every read here is guarded by name. A namespace-scoped credential — the usual
# production one — cannot `get nodes`, and an unguarded read ended the script
# with kubectl's own status 1: a red verdict about a cluster nobody judged.
node_runtime() { # node -> its runtime, or exit 2 naming why it is unknown
	local v
	v=$(kube_raw get node "$1" -o jsonpath='{.status.nodeInfo.containerRuntimeVersion}' 2> "$EVIDENCE/node-$1.err") \
		|| die "the runtime of node '$1' could not be read ($(tail -1 "$EVIDENCE/node-$1.err")): this credential may not read nodes, and this arm exists to report that runtime"
	[ -n "$v" ] || die "node '$1' reports no containerRuntimeVersion, so this arm cannot report what it measured"
	printf '%s' "$v"
}
NODE_OF=$(kube get pod "$PREFIX-bench" -o jsonpath='{.spec.nodeName}') \
	|| die "the node of the measured pod could not be read, so its runtime cannot be attributed"
RUNTIME=$(node_runtime "$NODE_OF") || exit 2
say "the pod ran on $NODE_OF: $RUNTIME"
for p in $ADMITTED; do
	n=$(kube get pod "$p" -o jsonpath='{.spec.nodeName}') || die "the node of $p could not be read"
	r=$(node_runtime "$n") || exit 2
	if [ "$r" != "$RUNTIME" ]; then
		say "FAIL $p ran on $n reporting '$r' while the measured pod ran on $NODE_OF reporting '$RUNTIME': two runtimes, so the comparison attributes nothing"
		exit 1
	fi
done

# --- verdict -----------------------------------------------------------------
rc=0
grep -q '^Seccomp:[[:space:]]*2' "$EVIDENCE/$PREFIX-posture.log" \
	|| { say "FAIL the RuntimeDefault arm carried NO seccomp filter, so it measured an unconfined container"; rc=1; }
# The trailing space is the anchor: the line is `uid=N gid=G`, and without it
# --uid 1000 accepted a readback running as 10001.
grep -q "^uid=$UID_WANT " "$EVIDENCE/$PREFIX-posture.log" \
	|| { say "FAIL the arm did not run as uid $UID_WANT, so it is not the $POSTURE_OF posture"; rc=1; }
grep -q '^NoNewPrivs:[[:space:]]*1' "$EVIDENCE/$PREFIX-posture.log" \
	|| { say "FAIL the measured arm lost no_new_privs, so it does not match the $POSTURE_OF securityContext"; rc=1; }

if [ "$HAVE_TWIN" -eq 1 ]; then
	grep -q '^Seccomp:[[:space:]]*0' "$EVIDENCE/$PREFIX-posture-unconfined.log" \
		|| { say "FAIL the Unconfined twin also reports a filter, so 'Seccomp: 2' above proves nothing"; rc=1; }
	grep -q '^NoNewPrivs:[[:space:]]*1' "$EVIDENCE/$PREFIX-posture-unconfined.log" \
		|| { say "FAIL the Unconfined twin lost no_new_privs too, so it varies two things and cannot attribute either"; rc=1; }
fi

case "$(exit_of "$PREFIX-bench")" in
0) ;;
2) say "nothing was tested: the bench pod refused its own arguments — a usage error says nothing about this cluster"; exit 2 ;;
4) say "nothing was tested: the bench pod is not the posture asked about"; exit 2 ;;
6) say "a leg could not run in this pod, so the deployment was not judged"; exit 2 ;;
7) say "the bench pod ran without checking its posture, so it says nothing about this cluster"; exit 2 ;;
"") say "the bench pod reported no exit code at all"; rc=1 ;;
*) say "FAIL the bench did not hold under this cluster's RuntimeDefault (exit $(exit_of "$PREFIX-bench"))"; rc=1 ;;
esac
grep -qE '^SELF .*sha256:[0-9a-f]{64}' "$EVIDENCE/$PREFIX-bench.log" \
	|| { say "FAIL the bench printed no SELF line carrying a digest, so this log cannot be attributed to a binary"; rc=1; }
grep -q 'VERDICT: 7/7 legs held' "$EVIDENCE/$PREFIX-bench.log" \
	|| { say "FAIL the bench pod did not hold, so nothing corroborates the profile the kubelet installed"; rc=1; }

# The kubelet merges the CONTAINER-level seccompProfile OVER the pod-level one.
# Reading only the pod-level field is a tautology twice over: it is the field
# this script wrote, and it is not the field that decides. The sentinel makes
# absence legible — jsonpath prints nothing for a missing container-level field,
# so an empty OUTPUT can only mean the read itself failed.
merged=$(kube get pod "$PREFIX-bench" -o jsonpath='pod={.spec.securityContext.seccompProfile.type}{range .spec.containers[*]} container={.securityContext.seccompProfile.type}{end}' 2>/dev/null)
case "$merged" in
"") say "FAIL the measured pod's seccompProfile could not be read at all, so nothing was identified"; rc=1 ;;
"pod=RuntimeDefault container=") ;;
"pod=RuntimeDefault container=RuntimeDefault") ;;
*) say "FAIL the kubelet applied something other than RuntimeDefault ($merged): it merges the CONTAINER-level field OVER the pod's"; rc=1 ;;
esac

# --- what this arm did not earn ---------------------------------------------
if [ "$LIVE_MATCH" = none ]; then
	say "GAP no sandbox run was live in '$NS', so the measured pod was not compared with a run's: its user is --sandbox-user's ('$SANDBOX_USER'), not read off the deployment"
fi
if [ "$HAVE_TWIN" -eq 0 ]; then
	say "GAP the Unconfined twin was refused by admission, so the seccomp read is uncorroborated: re-run in a namespace whose policy allows it (--create-namespace labels it privileged)"
fi
say "GAP the two Localhost seccomp mutants were NOT run: a Localhost profile lives in the kubelet's seccomp root on the node. The mutant control is green on the kind arm and is not re-earned here"

say "runtime measured: $RUNTIME"
[ "$rc" -eq 0 ] || { say "the remote arm failed. Evidence: $EVIDENCE"; exit "$rc"; }
say "the remote arm holds (mutants excluded, declared above). Evidence: $EVIDENCE"
