#!/usr/bin/env bash
# The docker arm of the codemode posture bench (#3155, ADR-165).
#
# WHY IT IS NOT A UNIT TEST. In the default cloud posture nothing is sandboxed:
# charts/iterion/values.yaml ships `runner.sandbox.enabled: false`, so the
# configmap renders `ITERION_SANDBOX_OVERRIDE: "none"` — "the pod is the
# isolation boundary". Codemode's confinement therefore comes from kernel
# primitives inside the child, and whether those primitives WORK is decided by
# the deployment, not by this repository: a container's seccomp profile can
# refuse the syscall Landlock is built on.
#
# This does NOT replace pkg/scripthost's suite, which fails loudly on a kernel
# without Landlock rather than going green (it skips only when a lane sets
# ITERION_CODEMODE_ALLOW_NO_LANDLOCK=1 to go on record). What that suite cannot
# do is judge the SHIPPED image under the DEPLOYMENT's posture: it compiles and
# runs on the host and knows nothing of the pod's securityContext.
#
# WHAT IT RUNS. The SHIPPED binary of the image you pass, inside that image,
# under the pod securityContext of charts/iterion/values.yaml. The posture is
# not trusted to the flags: `inpodbench -require-uid` reads the kernel's own
# answer and REFUSES to measure a container that is not confined, which is the
# only thing standing between "legs held" and a --privileged container
# reporting the same (measured: it did, before that flag existed).
#
# WHAT IT DOES NOT COVER.
#   - `RuntimeDefault` names whatever the RUNTIME defaults to, so this member
#     answers for docker only. The containerd arm is codemode-cluster-bench.sh.
#   - Production RUNNER pods do not run this image: they run
#     iterion-runner-devbox as uid 1000 (docs/ci-pipeline-topology.md,
#     Dockerfile.runner-devbox). Pass --uid to match whatever image you give
#     it. Measured, so it is not a guess: at uid 1000 every leg still holds, so
#     the uid does not carry the verdict — but that image has not been run.
set -Eeuo pipefail

# Lanched from anywhere: `go build ./scripts/...` needs the module root.
cd "$(git rev-parse --show-toplevel)"

# Like the sibling gate members, progress goes to stderr so that stdout stays
# the bench's own output.
say() { printf 'codemode-inpod-bench: %s\n' "$*" >&2; }
die() { say "$*"; exit 2; }

# ONE guard for every setup step. A naked command dying under `set -e` used to
# exit 1 — a red verdict about a deployment — and the fix had been applied site
# by site while the class stayed open elsewhere. Anything unhandled reaching
# here means nothing was tested, which is this repo's 2.
trap 'rc=$?; say "nothing was tested: a setup step failed at line $LINENO (exit $rc)"; exit 2' ERR

usage() {
	cat >&2 <<'USAGE'
usage: codemode-inpod-bench.sh --image <repo@sha256:…> [--uid N] [--mutant eperm|enosys]

  --image    the runner image, pinned by DIGEST. A tag would make the bench
             measure whatever moved under it, which is the one thing a gate
             may not do.
  --uid      the uid the image runs as (default 10001, the chart's runAsUser).
  --mutant   deny landlock_create_ruleset with this errno, to prove the bench
             can go red on the class it exists for. `eperm` is the shape a
             container profile gives a syscall it does not allow; `enosys` is
             the shape of a kernel that has no Landlock. Both must fail with
             the bench's posture-refused code, and nothing else counts.

  ITERION_CI_GATE_EVIDENCE=<dir> keeps the logs. It may not point inside this
  work tree: the bench binary is 14 MB and the repo has already paid for one
  of those left behind untracked.
USAGE
	exit 2
}

IMAGE="" MUTANT="" UID_WANT=10001
while [ $# -gt 0 ]; do
	case "$1" in
	--image)
		[ $# -ge 2 ] || die "--image needs a value"
		[ -z "$IMAGE" ] || die "--image given twice ('$IMAGE' then '$2'): refusing to guess which image you meant"
		IMAGE=$2
		shift 2
		;;
	--uid)
		[ $# -ge 2 ] || die "--uid needs a value"
		UID_WANT=$2
		shift 2
		;;
	--mutant)
		[ $# -ge 2 ] || die "--mutant needs a value"
		MUTANT=$2
		shift 2
		;;
	*) usage ;;
	esac
done
[ -n "$IMAGE" ] || usage
# A digest is 64 hex characters. `*@sha256:*` also matched `foo@sha256:` and
# any tag carrying the substring, which is how a gate ends up measuring a
# moving name.
case "$IMAGE" in
*@sha256:[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
*) die "--image must be pinned by a 64-hex digest, got '$IMAGE'" ;;
esac
case "$UID_WANT" in '' | *[!0-9]*) die "--uid must be a number, got '$UID_WANT'" ;; esac
case "$MUTANT" in "" | eperm | enosys) ;; *) usage ;; esac

EVIDENCE=${ITERION_CI_GATE_EVIDENCE:-$(mktemp -d)}
case "$(cd "$EVIDENCE" 2> /dev/null && pwd -P || echo "$EVIDENCE")" in
"$PWD" | "$PWD"/*) die "ITERION_CI_GATE_EVIDENCE points inside the work tree ($EVIDENCE); the 14 MB bench binary would be left there untracked" ;;
esac
mkdir -p "$EVIDENCE"

CONTAINER=codemode-inpod-bench-$$
cleanup() { docker rm -f "$CONTAINER" > /dev/null 2>&1 || true; }
trap cleanup EXIT
# Also BEFORE: a previous run killed with SIGKILL runs no trap, and the next
# run that draws the same $$ dies on a name conflict instead of measuring.
cleanup

say "building the bench — CGO_ENABLED=0, because a cgo-linked child cannot confine its own threads"
CGO_ENABLED=0 go build -o "$EVIDENCE/inpodbench" ./scripts/ci/inpodbench \
	|| die "the bench could not be built, so nothing was tested"
# `go build` honours the umask and `docker cp` preserves the mode: under a
# hardened umask the copy landed 0700 owned by the build uid, and the container
# (a different uid) could not exec it — which the mutant arm then counted as a
# success.
chmod 0755 "$EVIDENCE/inpodbench" || die "the bench binary could not be made executable"

SECCOMP=()
if [ -n "$MUTANT" ]; then
	errno=1
	[ "$MUTANT" = enosys ] && errno=38
	# The mutant must mutate ONE axis: Landlock. Everything the posture check
	# probes stays denied, so the mutant is refused for its Landlock denial
	# (exit 3) and not for being a permissive profile (exit 4) — which is what
	# happened twice, each time the probe set grew and this profile did not.
	#
	# So the set is READ FROM THE BENCH rather than restated here. One source,
	# no drift: `-print-probe-set` is exactly the list the check requires.
	probes=$("$EVIDENCE/inpodbench" -print-probe-set) \
		|| die "the bench would not print its probe set, so a mutant profile cannot be built from it"
	denied=""
	for probe in $probes; do
		denied="$denied{\"names\":[\"$probe\"],\"action\":\"SCMP_ACT_ERRNO\",\"errnoRet\":1},"
	done
	printf '{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[%s{"names":["landlock_create_ruleset"],"action":"SCMP_ACT_ERRNO","errnoRet":%s}]}\n' \
		"$denied" "$errno" > "$EVIDENCE/seccomp-$MUTANT.json"
	SECCOMP=(--security-opt "seccomp=$EVIDENCE/seccomp-$MUTANT.json")
	say "MUTANT $MUTANT: landlock_create_ruleset denied with errno $errno — this run MUST be refused"
fi

# The pod securityContext of charts/iterion/values.yaml: runAsUser/runAsGroup
# (fsGroup has no docker equivalent and is covered by the cluster arm),
# seccompProfile RuntimeDefault — which under docker is the daemon's default
# profile, hence no --security-opt seccomp here — allowPrivilegeEscalation
# false, capabilities drop ALL. Resources are values-prod.yaml's runner limits.
# /tmp is a tmpfs because this arm has no emptyDir; the pod's own scratch mount
# is the cluster arm's business.
# The teeth are the kernel's answer (the bench's fingerprint probe); the
# IDENTITY is the runtime's own record, and it takes BOTH of the daemon's and
# the container's: `docker info` describes what the daemon hands out by default
# and says nothing about this container, which could still have been given a
# profile on its run line. Measured: with a container alive under a custom
# profile, `docker info` still reported profile=builtin.
#
# Its own file, because the first version appended this to posture.txt and a
# later `| tee` of the same path truncated it away — `grep -c SecurityOptions`
# on a real honest run returned 0, which is the third site of that class.
check_profile_identity() {
	opts=$(docker info --format '{{json .SecurityOptions}}' 2> /dev/null || echo '[]')
	echo "docker SecurityOptions: $opts" > "$EVIDENCE/profile-identity.txt"
	case "$opts" in
	*'name=seccomp,profile=builtin'*) ;;
	*) die "this daemon is not handing out its builtin seccomp profile ($opts), so a verdict here would not be about RuntimeDefault" ;;
	esac
	local own
	own=$(docker inspect "$CONTAINER" --format '{{json .HostConfig.SecurityOpt}}' 2> /dev/null || echo 'null')
	echo "container SecurityOpt: $own" >> "$EVIDENCE/profile-identity.txt"
	case "$own" in
	*seccomp=*) die "the measured container was given its own seccomp profile ($own), so this is not the runtime default" ;;
	esac
}

say "starting $CONTAINER on $IMAGE"
# Guarded, not left to set -e: an unguarded failure here left docker's own
# exit code (125 on a bad --user) as the member's verdict.
docker run -d --rm --name "$CONTAINER" --entrypoint sleep \
	"${SECCOMP[@]}" \
	--security-opt no-new-privileges \
	--cap-drop ALL \
	--user "$UID_WANT:$UID_WANT" \
	--memory 8g --cpus 4 \
	--tmpfs /tmp:rw,size=512m \
	"$IMAGE" 900 > /dev/null || die "the measured container could not be started"

# NOT into /tmp: that is a tmpfs mount, and a copy under it lands in the image
# layer the mount shadows — docker cp then reports success for a file the
# container cannot see.
docker cp "$EVIDENCE/inpodbench" "$CONTAINER:/inpodbench" > /dev/null \
	|| die "the bench could not be copied into the container"

# Not for a mutant run: that one passes its own profile on purpose.
[ -n "$MUTANT" ] || check_profile_identity

# CapBnd is in here because a verdict cited it while the evidence did not
# carry it: the reader was fixed and neither witness was. CapInh/CapAmb/CapPrm
# come free and answer the next question a reader will have.
read_posture() { # container
	docker exec "$1" sh -c \
		'grep -E "^(Seccomp|Seccomp_filters|NoNewPrivs|Cap(Eff|Bnd|Inh|Amb|Prm)):" /proc/self/status; echo "uid=$(id -u) gid=$(id -g)"'
}
{
	echo "image: $IMAGE"
	echo "docker: $(docker version --format '{{.Server.Version}}' 2> /dev/null || echo unknown)"
	read_posture "$CONTAINER" || echo "the posture could not be read"
} | tee "$EVIDENCE/posture.txt" >&2

# The twin is what makes "Seccomp: 2" mean something: without a reading that
# says 0, a container silently running unconfined is indistinguishable from a
# confined one. The cluster arm always had it and three texts claimed this one
# did too.
# no-new-privileges STAYS on the twin: dropping both would vary two things at
# once, and then NoNewPrivs: 1 in the measured arm could not be attributed to
# allowPrivilegeEscalation: false. Only seccomp differs.
docker run --rm --security-opt seccomp=unconfined --security-opt no-new-privileges \
	--cap-drop ALL --user "$UID_WANT:$UID_WANT" \
	--entrypoint sh "$IMAGE" -c \
	'grep -E "^(Seccomp|Seccomp_filters|NoNewPrivs|Cap(Eff|Bnd|Inh|Amb|Prm)):" /proc/self/status; echo "uid=$(id -u) gid=$(id -g)"' \
	> "$EVIDENCE/posture-unconfined.txt" 2>&1 \
	|| die "the Unconfined twin could not be started, so nothing compares the measured arm's filter: $EVIDENCE/posture-unconfined.txt"
grep -q '^Seccomp:[[:space:]]*0' "$EVIDENCE/posture-unconfined.txt" \
	|| die "the Unconfined twin also reports a seccomp filter, so the measured arm's 'Seccomp: 2' proves nothing: $EVIDENCE/posture-unconfined.txt"
grep -q '^Seccomp:[[:space:]]*2' "$EVIDENCE/posture.txt" \
	|| die "the measured container carries NO seccomp filter: $EVIDENCE/posture.txt"
# The twin keeps no-new-privileges so that only seccomp differs. That is the
# claim; this is the guard for it, which did not exist.
grep -q '^NoNewPrivs:[[:space:]]*1' "$EVIDENCE/posture-unconfined.txt" \
	|| die "the Unconfined twin lost no_new_privs too, so it varies two things and cannot attribute either"

rc=0
docker exec -e TMPDIR=/tmp \
	"$CONTAINER" /inpodbench -binary /usr/local/bin/iterion -require-uid "$UID_WANT" \
	> "$EVIDENCE/bench.log" 2>&1 || rc=$?
cat "$EVIDENCE/bench.log"

# B3: the provenance line is the only thing tying this log to a binary and a
# kernel. The cluster arm asserts it; so does this one.
# A run that produced a verdict and no provenance is a RED verdict, not a
# refused precondition: `die` answers 2, which a gate reads as neutral. And the
# line must carry a DIGEST — describeSelf falls back to "unreadable", which the
# bare '^SELF ' pattern accepted.
if [ "$rc" -eq 0 ] && ! grep -qE '^SELF .*sha256:[0-9a-f]{64}' "$EVIDENCE/bench.log"; then
	say "the bench printed no SELF line carrying a digest, so this log cannot be attributed to a binary"
	exit 1
fi

# Exit codes are a CONTRACT (see scripts/ci/inpodbench/main.go): 0 every leg
# held, 1 a leg failed, 2 usage, 3 the child could not confine itself, 4 this
# container is not the posture we asked to measure, 5 a child ran with no
# Landlock domain because -allow-without-landlock accepted it, 6 a leg could
# not run here at all. A mutant is
# credited ONLY for 3 — "any non-zero" accepted a 126 from an unexecutable
# binary and a missing image as proof that the bench can see a confinement
# failure.
if [ -n "$MUTANT" ]; then
	case "$rc" in
	3) say "mutant $MUTANT was refused as it must be (exit 3). Evidence: $EVIDENCE" ;;
	# A blind bench is a RED verdict, not a refused precondition: `die` would
	# answer 2, the code this repo's gate members reserve for "nothing was
	# tested", and a mutant that passes is very much something that was tested.
	0)
		say "mutant $MUTANT PASSED, which means this bench cannot see the failure it exists for"
		exit 1
		;;
	*)
		say "mutant $MUTANT failed with $rc, not the posture-refused code 3: this run broke for some OTHER reason and proves nothing about the bench's reach"
		exit 1
		;;
	esac
	exit 0
fi

case "$rc" in
0) say "every leg held. Evidence: $EVIDENCE" ;;
# The repo's gate members answer 2 for "precondition refused, nothing was
# tested" (see mongo-conformance.sh). The bench's 4 means this container was
# not the posture asked about, which is exactly that — and passing 4 through
# would read as a red verdict about the deployment.
4) say "nothing was tested: this container is not the posture asked about. Evidence: $EVIDENCE"; exit 2 ;;
6) say "a leg could not run here (this container is too small for it), so the deployment was not judged. Evidence: $EVIDENCE"; exit 2 ;;
7) say "the bench ran without checking its posture, so it says nothing about a deployment. Evidence: $EVIDENCE"; exit 2 ;;
2) say "the bench refused its own arguments, so nothing was tested. Evidence: $EVIDENCE"; exit 2 ;;
5) say "a child ran with NO Landlock domain and -allow-without-landlock accepted it. Evidence: $EVIDENCE"; exit 1 ;;
*) say "the bench failed (exit $rc). Evidence: $EVIDENCE"; exit 1 ;;
esac
