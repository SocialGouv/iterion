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

## Agent flow

```sh
iterion-instances context --project "$PWD" --run "$RUN_ID" --json
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
real run must return 404, the context token must still match, and no unrelated
non-terminal run may share the instance. It snapshots the exact live binary
and effective process environment, sends SIGTERM without escalation, launches
the candidate with the same recipe and `ITERION_BIN`, then requires the real
mission endpoint to return its documented list payload. Any failed
postcondition triggers rollback to the frozen bytes. Transaction state is
available through:

```sh
iterion-instances deployment-status --project "$PWD" --json
```

Human-input gates, operator pauses, stopped instances, foreign processes and
global CLI replacement are outside bootstrap authority.

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
