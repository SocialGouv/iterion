#!/usr/bin/env bash
# vendor-check — the local member of CI's `vendor-check` job.
#
# One of the two required contexts that had NO local member at all (the other
# is mongo-conformance): `grep -n 'mod tidy\|mod verify\|mod vendor'
# Taskfile.yml` returned nothing before this, so "the full required-check set a
# workflow job can express runs locally" — the first blocking proof the
# operator direct-landing lane asks for — could not be true.
#
# It MUTATES: `go mod tidy` and `go mod vendor` rewrite go.mod, go.sum and
# vendor/. Two consequences the aggregate has to respect:
#
#   * it runs LAST, so no member after it compiles against a vendor tree this
#     one rewrote mid-run;
#   * it refuses a tree whose own paths are already dirty, because
#     `git diff --exit-code` cannot tell "tidy changed this" from "somebody was
#     editing it" — and this repository is worked in parallel in one checkout,
#     so blaming another session's work in progress is the likely failure, not
#     the unlikely one.
#
# Exit codes: 0 clean · 1 the tree and the manifests disagree · 2 a
# precondition refused (which is NOT a gate verdict and must not be read as
# one).
set -euo pipefail

say() { printf '%s\n' "$*" >&2; }

cd "$(git rev-parse --show-toplevel)"

# The precondition, asserted rather than trusted.
dirty=$(git status --porcelain -- go.mod go.sum vendor/ || true)
if [ -n "$dirty" ]; then
	say "vendor-check: go.mod, go.sum or vendor/ already carries uncommitted changes:"
	say "$dirty"
	say ""
	say "This member rewrites those paths and then asks git whether anything moved, so it"
	say "cannot run over work in progress without blaming it. Commit or set aside the lines"
	say "above and run it again. Refusing is not a verdict: the gate has NOT passed."
	exit 2
fi

# Mirrors the job's two steps, in its order. `go mod verify` first, because a
# checksum mismatch is a different fact from a stale manifest and reporting
# the second would hide the first.
say "vendor-check: go mod verify"
go mod verify

say "vendor-check: go mod tidy && go mod vendor"
go mod tidy
go mod vendor

# The job's own comment: this fails when the previous two commands changed
# anything, which is exactly the condition to surface rather than let it show
# up later as a build skew.
if ! git diff --exit-code go.mod go.sum vendor/; then
	say ""
	say "vendor-check: go mod tidy / go mod vendor changed the tree — the diff above is what"
	say "CI would report. Commit it as part of this change."
	exit 1
fi

say "vendor-check: clean"
