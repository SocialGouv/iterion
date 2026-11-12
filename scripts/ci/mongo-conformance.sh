#!/usr/bin/env bash
# mongo-conformance — the local member of CI's `mongo-conformance` job.
#
# The second of the two required contexts that had no local member: one `mongo`
# in the whole Taskfile, and it belonged to `cloud:up:deps`. So the lane's
# first proof could not be true.
#
# THE PACKAGE LIST IS READ FROM THE JOB, never restated here. A copy would
# drift the hour someone adds a Mongo-gated package, and the drift's failure
# mode is the worst one: a suite nobody runs locally, green by absence. The
# job's own header says the same thing about itself.
#
# Both of the job's `go test` invocations run, not just the first. The recipe in
# docs/development.md extracts `\./pkg/[a-z/]+/\.\.\.`, which cannot see the
# second line — `./pkg/server/ -run '^TestRound3CloudCoordinatorBusHoldRecovery$'`,
# the public cloud coordinator against the durable outbox. A member that ran
# only the first would report a verdict for a job it had not finished.
#
# Exit codes: 0 every invocation green · 1 at least one red (named, with its
# log) · 2 a precondition refused, and the list is longer than it looks — each
# path a machine that cannot judge rather than a tree that is broken, and all
# of them greppable (`grep -cE '^[[:space:]]+exit 2'` — nine, where a plain
# `grep -c 'exit 2'` counts ten by matching this sentence): python3 unable to read the job
# (devbox pins none), the job's
# shape not recognised, timeout(1) missing, docker missing, its daemon not
# answering, the container refusing to start, mongod accepting no connection
# on the published port, the replica set never elected, the port unreachable
# from this process, and the member not finishing its own plan.
# A refusal is NOT a verdict.
set -euo pipefail

say() { printf '%s\n' "$*" >&2; }

cd "$(git rev-parse --show-toplevel)"

WORKFLOW=.github/workflows/tests.yml
CONTAINER=iterion-mongo-conf-local
PORT=27018
EVIDENCE=${ITERION_CI_GATE_EVIDENCE:-$(mktemp -d)}
mkdir -p "$EVIDENCE"

# ---------------------------------------------------------------------------
# The invocations, derived from the job. FIRST, before docker: deriving needs
# no container, and a member that cannot tell what to run must say so on any
# machine rather than only on one that could have run it.
# ---------------------------------------------------------------------------
# python3 rather than sed+grep: the job's run block is YAML with line
# continuations, and reconstructing a command from a line-oriented scan is how
# the second invocation got lost. Parsing the block and splitting on the
# commands it contains keeps the member honest about what the job runs.
PYERR=$(mktemp)
trap 'rm -f "$PYERR"' EXIT
mapfile -t INVOCATIONS < <(python3 -I - "$WORKFLOW" 2> "$PYERR" <<'PY'
import re, sys, yaml

doc = yaml.safe_load(open(sys.argv[1]))
job = doc.get("jobs", {}).get("mongo-conformance")
if job is None:
    sys.exit("the workflow defines no mongo-conformance job")

blocks = [s["run"] for s in job.get("steps", []) if isinstance(s, dict) and s.get("run")]
# The harness step is the one that runs go test; the mongo start-up and the
# failure diagnostics are not the gate.
found = []
for b in blocks:
    # Join the shell's line continuations, drop comments, then split on the
    # commands themselves.
    joined = re.sub(r"\\\n\s*", " ", b)
    for line in joined.splitlines():
        line = line.strip()
        if line.startswith("#") or "go test" not in line:
            continue
        found.append(re.sub(r"\s+", " ", line))
if not found:
    sys.exit("the mongo-conformance job runs no `go test`: its shape changed and this member "
             "cannot derive what to run — teach it rather than guessing")
print("\n".join(found))
PY
)

# The COUNT, never the status of the `||`. A `mapfile < <(python3 …)` reports
# mapfile's own success: the process substitution hides python3's exit from
# `set -e` and from `pipefail` alike, so the refusal this script computes and
# prints would be thrown away, the loop below would run zero invocations, and
# `exit "$failed"` would answer 0 — a REQUIRED context reporting green over
# nothing tested. Measured on this file before the guard existed. python3 only
# prints when it succeeds, so the count closes the class; it is the same shape
# scripts/desktop/nix-pkgconfig-env.sh already uses after its own `mapfile`.
[ "${#INVOCATIONS[@]}" -gt 0 ] || {
	say "mongo-conformance: derived NO invocation from $WORKFLOW."
	# The reason is python3's, CAPTURED rather than assumed: a wrapper that
	# dies without writing anything would otherwise leave the refusal citing
	# an explanation nobody printed.
	if [ -s "$PYERR" ]; then
		sed 's/^/  /' "$PYERR" >&2
	else
		say "  (python3 printed no reason — it may have failed before running: check its wrapper)"
	fi
	say "Refusing is not a verdict: nothing was tested."
	exit 2
}

say "mongo-conformance: ${#INVOCATIONS[@]} invocation(s) derived from $WORKFLOW"

command -v docker > /dev/null 2>&1 || {
	say "mongo-conformance: docker is not on PATH, and this member needs a real replica set"
	say "(change streams and transactions). Refusing is not a verdict."
	exit 2
}
# The BINARY is not the service, and this is the frequent case rather than the
# exotic one: a stopped daemon, a user outside the docker group, a rootless
# daemon that was never started. Without this probe `docker run` fails under
# `set -e` and the member exits 1 — "a member is red" — about a machine that
# cannot judge at all.
# Bounded: a socket that accepts and then says nothing would hang this member
# for as long as the operator is willing to wait, where every wait below has a
# ceiling. A warm `docker info` here is 0.03 s (five runs), so 20 s is ~650x
# it — sized for a cold daemon, not for the usual case.
#
# `timeout` is a HOST binary: devbox pins no coreutils, so it can be missing
# (a BSD userland, a slim container image). Checked, because without this the
# absence arrives as "the daemon did not answer" with the real reason
# swallowed by the redirection — a refusal pointing at the wrong thing.
command -v timeout > /dev/null 2>&1 || {
	say "mongo-conformance: timeout(1) is not on PATH, so the docker probe below cannot be bounded"
	say "and a hung daemon would hang this member. devbox pins no coreutils; install one or run"
	say "this where timeout exists. Refusing is not a verdict."
	exit 2
}
timeout 20 docker info > /dev/null 2>&1 || {
	say "mongo-conformance: docker is on PATH but its daemon did not answer within 20 s (stopped,"
	say "unreachable socket, or hung). Refusing is not a verdict."
	exit 2
}

# ---------------------------------------------------------------------------
# The replica set. Three details that each cost a session — see
# docs/development.md; they are repeated as code, not as a comment pointing at
# a comment.
# ---------------------------------------------------------------------------
# Both, because a second `trap … EXIT` REPLACES the first: the earlier one
# removed $PYERR, and installing this one alone would leak it.
cleanup() {
	docker rm -f "$CONTAINER" > /dev/null 2>&1 || true
	rm -f "$PYERR"
}
trap cleanup EXIT

cleanup
# mongod listens on the PUBLISHED port inside the container too, so the member
# it advertises resolves to itself. `-p 27018:27017` with a member advertised
# as localhost:27018 never initiates. And --ulimit nofile is not optional: the
# suites create a fresh database with ~10 indexes per subtest, and a default
# descriptor limit makes mongod die with "Too many open files" minutes in,
# which the tests then report as "connection refused".
# Guarded rather than left to `set -e`: an image this host cannot pull (no
# network) or a port already taken is a machine that cannot judge, not a red
# tree.
docker run --rm -d --name "$CONTAINER" --ulimit nofile=131072:131072 \
	-p "127.0.0.1:$PORT:$PORT" mongo:8.0 \
	--replSet rs0 --bind_ip_all --port "$PORT" > /dev/null || {
	say "mongo-conformance: the mongo:8.0 container would not start (an image this host cannot"
	say "pull, or port $PORT already taken). Refusing is not a verdict."
	exit 2
}

# Wait for mongod to ACCEPT A CONNECTION before initiating. `docker run -d`
# returns as soon as the container exists, not when the server is listening,
# and `rs.initiate` against a socket nobody is on answers
# `MongoNetworkError: connect ECONNREFUSED` — measured, on the first run of
# this script. The CI job polls the same ping for the same reason; leaving it
# out is how a local member reports a refusal for a replica set that was
# merely slower than the next line.
up=""
for _ in $(seq 1 30); do
	if docker exec "$CONTAINER" mongosh --port "$PORT" --quiet \
		--eval 'db.runCommand({ping:1})' > /dev/null 2>&1; then
		up=yes
		break
	fi
	sleep 2
done
if [ -z "$up" ]; then
	say "mongo-conformance: mongod never accepted a connection on port $PORT."
	docker logs "$CONTAINER" > "$EVIDENCE/mongod.log" 2>&1 || true
	say "mongod's log: $EVIDENCE/mongod.log. Refusing is not a verdict: nothing was tested."
	exit 2
fi

docker exec "$CONTAINER" mongosh --port "$PORT" --quiet \
	--eval "try { rs.status() } catch (e) { rs.initiate({ _id: 'rs0', members: [{ _id: 0, host: 'localhost:$PORT' }] }) }" \
	> /dev/null

# POLL for PRIMARY. A single status read is not a wait: go test connecting
# mid-election answers ReplicaSetNoPrimary, which reads as a store regression
# on whatever change happens to be in the tree.
state=""
for _ in $(seq 1 30); do
	state=$(docker exec "$CONTAINER" mongosh --port "$PORT" --quiet \
		--eval 'try { print(rs.status().members[0].stateStr) } catch (e) { print("NOTREADY") }' 2> /dev/null | tail -1 || true)
	[ "$state" = "PRIMARY" ] && break
	sleep 2
done
if [ "$state" != "PRIMARY" ]; then
	say "mongo-conformance: the replica set never became PRIMARY (last state: ${state:-none})."
	say "Refusing is not a verdict: nothing was tested."
	docker logs "$CONTAINER" > "$EVIDENCE/mongod.log" 2>&1 || true
	say "mongod's log: $EVIDENCE/mongod.log"
	exit 2
fi
say "mongo-conformance: rs0 primary elected on port $PORT"

# Probe the path THE TESTS take, not the one docker exec takes.
#
# `docker exec mongosh` runs INSIDE the container and proves only that mongod
# is alive. The suites connect from this process's network namespace to the
# PUBLISHED port, and those are not the same route: measured 2026-11-12, a
# command confined by Claude Code's bubblewrap sandbox gets its own loopback,
# so `docker exec … ping` answered ok while a plain TCP connect to
# 127.0.0.1:<port> was refused. Without this probe the member then spent 60
# seconds per package tree failing with `ReplicaSetNoPrimary … connection
# refused` — 29 trees of nothing, reported in a shape that reads as a store
# regression on whatever change happens to be in the tree.
#
# The container's own IP is not a way round it: measured the same day,
# 172.17.0.2:27020 answered `Network is unreachable` from the same shell. The
# sandbox reaches no docker network at all.
#
# A control that certifies the wrong route certifies nothing.
if ! python3 -I -c "
import socket, sys
s = socket.socket(); s.settimeout(5)
try:
    s.connect(('127.0.0.1', $PORT))
except Exception as e:
    print(type(e).__name__ + ': ' + str(e), file=sys.stderr); sys.exit(1)
" 2> "$EVIDENCE/port-probe.err"; then
	say "mongo-conformance: this process cannot reach the published port 127.0.0.1:$PORT:"
	say "  $(cat "$EVIDENCE/port-probe.err")"
	say ""
	say "mongod is alive — docker exec reached it — but the suites connect from HERE, and this"
	say "network namespace has no route to a host-published port. A confined shell (bubblewrap,"
	say "a container without host networking, a restricted CI runner) looks exactly like this."
	say "Run the member from a plain shell on the host, or give this one host networking."
	say "Refusing is not a verdict: nothing was tested."
	exit 2
fi
say "mongo-conformance: the published port answers from this process too"

# ---------------------------------------------------------------------------
# Run each invocation, read its own exit code, keep going.
# ---------------------------------------------------------------------------
export ITERION_TEST_MONGO_URI="mongodb://localhost:$PORT/?replicaSet=rs0"
failed=0
i=0
: > "$EVIDENCE/mongo-conformance.matrix"
for inv in "${INVOCATIONS[@]}"; do
	i=$((i + 1))
	log="$EVIDENCE/mongo-conformance.$i.log"
	say "mongo-conformance: [$i/${#INVOCATIONS[@]}] $inv"
	# Redirected to a FILE and the code read on the very next line. Never a
	# pipe on a gate command: `| head` closes the pipe, SIGPIPE kills the run,
	# and the status read is head's — this repository announced a green Go
	# suite over a red package that way.
	set +e
	eval "$inv" > "$log" 2>&1
	rc=$?
	set -e
	printf '%d\t%d\t%s\t%s\n' "$i" "$rc" "$log" "$inv" >> "$EVIDENCE/mongo-conformance.matrix"
	if [ "$rc" -ne 0 ]; then
		failed=1
		say "mongo-conformance: invocation $i FAILED (rc=$rc) — $log"
		grep -E '^(--- FAIL|FAIL|panic:)' "$log" | head -20 >&2 || true
	fi
done

say ""
# Counted on the MATRIX, not on the loop variable: `i` is incremented inside
# the body, so it could only ever equal the plan and the comparison below
# would be decorative. The matrix holds one line per invocation whose exit
# code was actually recorded.
ran=$(wc -l < "$EVIDENCE/mongo-conformance.matrix")
say "mongo-conformance: ran $ran of ${#INVOCATIONS[@]} invocation(s); matrix at $EVIDENCE/mongo-conformance.matrix"
cat "$EVIDENCE/mongo-conformance.matrix" >&2
if [ "$ran" -ne "${#INVOCATIONS[@]}" ]; then
	say "mongo-conformance: ran $ran of ${#INVOCATIONS[@]} — the member did not finish its own plan."
	exit 2
fi
exit "$failed"
