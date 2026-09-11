# Project-scoped local instance bootstrap

`iterion-instances` is the host-side owner for local Studio processes. It keeps
project identity in the project, while ports, stores, source repositories and
deployment state remain host-owned.

## Configuration boundary

Every managed project has a deliberately small `iterion.project.toml`:

```toml
schema_version = 1
project_id = "shorts"
deployment_profile = "iterion-local-development"
```

The manifest accepts no path, command, port, secret or authorization field.
`~/.config/iterion/instances.conf` remains the launch recipe. The adjacent
`deployment-profiles.toml` binds an instance name to the manifest and defines
the Iterion source ref plus the root under which session worktrees may be
created. Project lookup is an exact canonical-path match; ports are never
scanned to guess ownership.

When migrating a pre-existing helper without restarting its Studios, preserve
each managed process's exact executable in an immutable per-instance slot:

```sh
iterion-instances adopt-active town shorts --json
```

Future ordinary starts then keep the instance-specific binary until a verified
bootstrap deployment supersedes it.

A one-time migration of a foreign process requires an operator-resolved PID,
an exact launch-recipe match and a matching canonical `server/info` work dir:

```sh
iterion-instances adopt-process --project /absolute/project --pid PID --json
```

The manager never scans ports to infer that PID.

## Agent flow

```sh
iterion-instances context --run "$RUN_ID" --json
iterion-instances prepare --project "$PWD" --session "$SESSION_ID" --json
# Work only in the returned worktree. Commit the repair there.
iterion-instances build --project "$PWD" --session "$SESSION_ID" --commit "$FULL_SHA" --json
iterion-instances deploy --project "$PWD" --artifact "$ARTIFACT_ID" \
  --expect-context "$CONTEXT_TOKEN" --goal "$GOAL_ID" --run "$RUN_ID" --json
```

`build` accepts only the exact `HEAD` commit of the prepared, clean worktree.
It rejects `.env` and pre-existing `.task/`, runs only `devbox run -- task
build`, requires the output basename `iterion`, records tool versions, and
starts the candidate against temporary project/store directories with
`--recovery-passive` before sealing it.

`deploy` is narrower than ordinary instance administration: the project must
already be running under this manager, the assistant-missions probe for the
real run must return 404 and the context token must still match. Admission uses
the authoritative lifecycle vocabulary: unrelated `running` and `queued` runs
block; `finished`, `failed`, `failed_resumable`, `cancelled`,
`paused_waiting_human`, and `paused_operator` rows are restart-admissible.
Those last two statuses remain exact blockers when they belong to the target
run itself. Unknown statuses, malformed rows, duplicate or missing IDs, and
incomplete inventories fail closed.

`context` performs one unfiltered `/api/runs` read, validates every served
`RunSummary`, and binds its canonical fingerprint plus the persisted dispatcher
intent from `<store>/dispatcher/runtime.json` into the context token. The
fingerprint covers `id`, `status`, `updated_at`, `finished_at`, `end_reason`,
and `failure_code`; absent, null, and empty optional values canonicalize to the
same empty value. Diagnostics are bounded, but admission always examines the
complete served list.

Immediately before SIGTERM, `deploy` rechecks the inventory and dispatcher
intent and writes a private transaction receipt. It snapshots the exact live
binary and effective process environment, drains without escalation, launches
the candidate with the same recipe and `ITERION_BIN`, then compares every
pre-existing unrelated run after readiness and again after the real mission
probe. There is no HTTP inventory read while the process is down. New active
or unknown rows and any changed or disappeared pre-existing unrelated record
fail deployment; new known terminal or dormant rows are retained and reported.

The replacement and rollback binaries both follow the persisted dispatcher
intent; bootstrap does not invent a passive-start barrier or require an old
Studio dispatcher-control route. Rollback restores exact executable bytes and
environment, but never overwrites run records, reverts operator intent, deletes
created runs, or claims those effects were undone. The journal distinguishes
process restoration from a detected or unverifiable preservation failure.
Transaction state is available through:

```sh
iterion-instances deployment-status --project "$PWD" --json
```

When `context` starts outside a configured project folder, `--run` discovers
the target only among already-managed configured instances. A candidate must
return that run and report a canonical `server.info.work_dir` equal to its
configured project path; zero or multiple matches fail closed.

An exact `paused_waiting_human` or `paused_operator` status on the target,
verified operator stops, stopped instances, foreign processes and global CLI
replacement are outside bootstrap authority. An unrelated dormant row does not
become a target gate merely because it is waiting at a healthy chat boundary.

## Installation and tests

Install only from a committed, clean Iterion worktree:

```sh
python3 scripts/local-instance-manager/install.py
```

Run the standard-library tests explicitly:

```sh
ITERION_TEST_TMPDIR=/var/tmp python3 -m unittest discover \
  -s scripts/local-instance-manager -p 'test_*.py'
```
