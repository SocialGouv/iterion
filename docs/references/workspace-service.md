# Shared local workspace service

Local development uses one per-user service, `iterion-workspace.service`, for
all registered projects. The default listener is `http://127.0.0.1:4891` and a
project is addressed through its registry ID:

```text
http://127.0.0.1:4891/x/<project-id>/
```

Use `iterion-workspacectl` instead of starting a Studio process for a project:

```bash
iterion-workspacectl status
iterion-workspacectl list --json
iterion-workspacectl context --project "$PWD" --json
iterion-workspacectl open --project "$PWD"
iterion-workspacectl logs --lines 200
```

`context` resolves the deepest registered directory containing the supplied
path. It does not change the current project. Use `--current` only when the
desktop's current selection is intentionally wanted.

`start`, `stop`, and `restart` always affect the shared service and therefore
every registered project. Before an ordinary stop or restart, the controller
queries every store-backed project for all `running` and `queued` runs twice.
It refuses the operation when work exists or any query is incomplete. `--force`
is reserved for recovery and bypasses that protection. The advisory lock
serializes controller calls; the server has no admission barrier, so a separate
client can still submit a run between the last check and the systemd operation.
Paused runs are preserved on disk but do not block these operations.

Machine output uses a versioned envelope:

```json
{"schema_version":1,"ok":true,"command":"context","result":{}}
```

Errors use exit status 2 and replace `result` with a structured `error`.
Warnings and human diagnostics go to stderr, leaving JSON stdout clean.

`iterion-instances` remains as a compatibility launcher for common
start/stop/restart/status/list/context/open/logs calls. A project argument only
selects displayed context; it never narrows a service mutation. Its old
prepare/build/deploy and process-adoption commands are retired.

The controller reads `~/.config/Iterion/config.json`; ordinary commands never
write it. The systemd unit must not set `ITERION_INSTANCES_CONFIG`. Because the
server otherwise checks the fallback `~/.config/iterion/instances.conf`, that
file must be absent or contain comments only.

Run the focused tests with:

```bash
python3 -m unittest discover -s scripts/tests -p 'test_*.py'
```
