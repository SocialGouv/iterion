# Probes and graceful shutdown

How a k8s deployment of iterion answers *is this pod alive*, *should it
receive traffic*, and *what happens when it is asked to leave*.

Read this when:

- a rolling deploy or an autoscale event produces 502s, or a forge webhook
  is delivered but no run starts;
- a pod CrashLoops at boot with nothing in the logs but a startup sequence;
- a runner pod shows `Ready` while the queue is not moving;
- you need to size `terminationGracePeriodSeconds` after changing a delay.

## The four endpoints

| Surface | Endpoint | Answers 503 when | Never fails on |
|---|---|---|---|
| server | `GET /healthz` | never | anything — it is a mux-liveness signal only |
| server | `GET /readyz` | draining, superseded rollout epoch, or a **critical** dependency is down | a non-critical dependency (reported as `degraded`, still 200) |
| runner | `GET /healthz` (metrics port) | the idle consume loop stopped cycling | a run being in flight, however long |
| runner | `GET /readyz` (metrics port) | starting, draining, or superseded rollout epoch | — |

Both server endpoints ride the API port; both runner endpoints ride the
metrics port (9090), which the chart already exposed. All four return a
JSON envelope carrying `version`/`commit`, so a probe response also tells
you which build answered.

### Why `/readyz` does not gate on every dependency

Every replica pings the same Mongo, the same NATS, the same S3. If all of
them gate readiness, a 15-second Mongo failover removes *every* pod from
the Service at once and the ingress 503s everything — including the routes
that never touch Mongo. A partial degradation becomes a total outage, and
the pods take an extra readiness period to come back after the backend
does.

So a dependency is marked `Critical` only when the pod genuinely cannot
serve without it. Today that is **Mongo alone**
([cmd/iterion/server.go](../cmd/iterion/server.go), the `ReadinessChecks`
map). NATS, S3 and Valkey are pinged, reported, and *not* fatal:

```json
{"status":"degraded","mode":"cloud","version":"v3.52.0","commit":"a984b1066",
 "usage_cap":"usage caps off",
 "checks":{"mongo":"ok","s3":"error: connection refused","nats":"ok","valkey":"ok"}}
```

(`version` / `commit` / `usage_cap` ride every health response, so a probe
also tells you which build answered and whether the usage cap reached the
deployment. `"valkey":"ok"` means reachable **or not configured** — the
check returns nil when no Redis client is wired.)

That body comes back with **200**. Alerting is what should page on it —
[docs/observability.md](observability.md) — because a degraded backend no
longer trips a `/readyz` 503 alert.

The dependencies are pinged **concurrently**, each under its own 1s
deadline, so the probe answers in `max(ping)` rather than `sum(ping)`.
That is why `probes.readiness.timeoutSeconds` is 3 and not 1: the kubelet
must not abort a probe that was about to report a merely-degraded
backend, which would fail readiness on exactly the case the split exists
to survive.

A ping that *panics* (a driver bug, a malformed response) is reported as
`"panic: <value>"` in the same map and counts as a failure for its own
`Critical` flag — it is caught rather than allowed to exit the process,
which would be an un-drained shutdown, the very thing this page is about.
The panic is also sent to the error tracker.

A ping that never returns at all (a driver ignoring its context) is
reported as `"timeout: no answer within …"`, and the **next** probes say
`"stalled: an earlier probe's ping has not returned"` — the wedged check
is not launched a second time. That matters more than it looks:
re-launching it every five seconds strands two goroutines each time
(~100 MB/day, measured), and the pod eventually dies by OOMKill — a
SIGKILL, so no drain, no lame-duck window. One captive goroutine per
dependency is the price; a growing pile of them is not.

The flip side is that **overlapping probes share one ping** rather than
each starting their own: a kubelet tick next to an operator's `curl`, or
an LB health check, all read the same answer. Reporting the second caller
as stalled would 503 a perfectly healthy pod — and in a mechanism that
evicts, a false positive costs as much as a missed failure.

Each check is reported from a single atomic observation, so a ping that
completed is never published as a timeout. What no design removes is the
boundary itself: a dependency answering in the same microsecond the
deadline expires is a coin flip, like any timeout. With a 1.2s budget
against a Mongo answering in milliseconds that window is negligible, and
`failureThreshold: 3` means it would have to land three probes in a row.

### Why the runner's liveness is not a TCP check

A `tcpSocket` probe on the metrics port proves a socket is listening,
which the process does from boot to exit. A runner whose JetStream
consumer died stayed `Ready` forever, consuming nothing.

`/healthz` reports the consume **loop** instead
([pkg/runner/health.go](../pkg/runner/health.go)):

- a run in flight → **alive** (the loop is blocked inside `processOne` by
  design, for hours; killing that pod throws away checkpointable work — the
  drain ceiling and the budget are what bound a run, not the kubelet);
- idle and cycling → **alive**. The threshold is 2 minutes or 20× the
  loop's `FetchWait`, whichever is longer, so raising the fetch cadence
  never turns a healthy long-poll into a restart;
- idle and not cycling → **stalled**, and a restart genuinely fixes it.

`/readyz` is the drain signal: a runner in its lame-duck can hold a pod for
up to `config.runner.drainTimeout` (8h), and `kubectl get pods` should
distinguish it from a fresh one.

## The shutdown sequence (server)

```
pod marked Terminating
├─ endpoints controller starts removing the pod   ← asynchronous, 1–10s
├─ preStop hook (probes.preStopDelaySeconds, default 0 = disabled)
└─ SIGTERM
   ├─ /readyz → 503 {"status":"draining"}         ← immediately
   ├─ wait config.server.shutdownDelay (5s)       ← the lame-duck window
   ├─ run-console drain      ⎫ config.server.shutdownTeardown (30s),
   ├─ http.Server.Shutdown   ⎭ split ⅔ / ⅓ so in-flight requests keep a window
   └─ background join        ← 500ms, one budget for every background goroutine
```

The lame-duck window is the whole point. Endpoint removal is asynchronous:
without the pause the listener closes while traffic is still being routed
to the pod, which is a connection-refused — a 502 for a studio user, a
dropped delivery for a forge webhook. With `server.hpa.enabled` (the
default) this happens on every scale-down, not just on deploys.

`/healthz` deliberately stays **200** through the drain: a liveness failure
would make the kubelet kill the pod in the middle of it.

Once the sequence starts it runs to completion or to its own deadline — a
second SIGTERM (or a second Ctrl-C) does not shorten it. Locally that is
why `ITERION_SHUTDOWN_DELAY` defaults to 0: a studio you interrupt must
stop now. A zero delay still flips `/readyz`; it only removes the pause,
so the probe stays honest for however long the teardown takes.

**Known limitation.** `http.Server.Shutdown` does not close hijacked
connections, and the run-console / shell / browser WebSockets
([pkg/server/runs_ws.go](../pkg/server/runs_ws.go) and siblings) do not
watch the shutdown signal — only the studio's `/api/ws` hub does. Those
sockets therefore get a TCP reset at process exit rather than a close
frame; clients reconnect, so it is cosmetic, but a deploy is visible in a
watching browser.

### The background join

After the HTTP drain, the shutdown JOINS every goroutine the server registered
— the sweeps and lease campaigns started through `goUntilShutdown`, and the
detached best-effort writes of `goSafe`: an audit row, a `last_used_at` stamp,
an invitation email. Cancelling them would not be enough: a loop stopped by a
cancel alone returns after its cancel did, so the process could exit between a
claim and its release, or mid-insert into a store already being torn down, and
the grace period would never have covered that last write.

ONE budget covers them all (`defaultBackgroundJoinBudget`, 500ms; a server may
override it through `bgJoinBudget`), spent INSIDE the window above rather than
added to it — so the exit costs the same whether the server started two
background goroutines or twenty. Whatever is still running when the budget runs
out is named in a warning: past that point the process is leaving, and the only
thing an operator can act on is WHICH one overran — so each task is named ONCE,
with how many of its goroutines were still running, because every per-request
write goes through the same registry and a burst of them under one label would
otherwise be a multi-kilobyte log line.

That budget is a WINDOW, not a guarantee, and it is far shorter than what the
sites it joins allow themselves: 5s for an audit insert or a `last_used_at`
stamp, 15s for an invitation email, 30s for each half of the forge reviewer
settle and for the model-spec refresh's aggregator fetch, 60s for the OAuth
connect kick — and no deadline at all for the config-share touch, the one
joined write that takes none. A write that completes inside the window
finishes; one that does not is cut by the exit exactly as it was before the
join existed, and the warning names it. The join
NARROWS the window in which a write is lost — it does not close it — and the
lever that widens it is the grace-period arithmetic below, not a deadline at
the site.

A goroutine the join is waiting for may ask for one last write on its way out —
a sweep's final audit row — and that write is joined too, out of the same
budget: the registry is re-read until it is empty, so a chain of such writes
cannot stretch the exit beyond the one window either.

Work asked for AFTER the join is refused and logged rather than started —
nobody is left to wait for it. That is a claim about the ORDER, so the join is
the sequence's last wait: the board dispatcher's drain — the one other thing
`Shutdown` waits for, while its in-flight cards release their claims — is
waited for *before* it, because a card finishing there can ask for one more
best-effort write, and a registry closed a step early would refuse it with the
process demonstrably still alive. The drain window — between the shutdown signal
and `http.Server.Shutdown` returning — splits by registration path:

- a WRITE (`goSafe`) is still started and still joined. Requests are answered
  until the drain ends and the join comes after it, so the audit row of a
  mutation answered 200 during a rolling update is covered. It carries its own
  deadline, never the shutdown's, because the point is to let that last write
  finish inside the grace period rather than cut it there.
- a LOOP (`goUntilShutdown` / `tryGoUntilShutdown`) is refused on the signal
  and says so: it would run on an already-cancelled context, with every store
  call in it failing as a generic `context canceled`. Its caller records the
  work as deferred to its reconciliation sweep instead — what the forge→board
  projection does with a webhook answered in that window.

The census of every goroutine `pkg/server` starts, each with what joins it or
why the join is not owed, is a test:
[pkg/server/background_class_test.go](../pkg/server/background_class_test.go).

### Sizing `terminationGracePeriodSeconds`

```
probes.preStopDelaySeconds
  + config.server.shutdownDelay
  + config.server.shutdownTeardown
        ≤ server.terminationGracePeriodSeconds
```

Defaults: `0 + 5 + 30 = 35 ≤ 60`. Widen any of the three and raise the
grace period by as much, or the kubelet SIGKILLs the pod mid-teardown.
Raise `shutdownTeardown` when the deployment serves long requests (a big
artifact upload, a streamed response) that a 30s cut would truncate on
every deploy.

The runner has its own, much larger arithmetic — it waits out a whole run:
`runner.terminationGracePeriodSeconds` (29100s) ≥
`config.runner.drainTimeout` (8h) + checkpoint margin. See
[docs/cloud-deployment.md](cloud-deployment.md).

### When to enable the preStop hook

`probes.preStopDelaySeconds` is 0 (disabled) by default: the in-process
window already covers the standard endpoints-based path, and a preStop
sleep runs *before* SIGTERM, so its seconds **add** to the delay rather
than overlapping it.

Turn it on for an ingress controller that routes to the Service ClusterIP
instead of to endpoints (nginx's `service-upstream` annotation), where a
readiness flip alone does not divert traffic.

## The startup probe

A cloud server connects NATS + S3 + Mongo, ensures the schema of ~28
collections, seeds the marketplace and reconciles the bootstrap admin —
**before** it binds its listener. On a cold or contended Mongo that can
outlast the liveness budget (`10s + 3×10s`), and the kubelet kills the pod
into a CrashLoop that reads like an application bug.

`probes.startup` (enabled, 30 × 5s = 150s) suspends liveness *and*
readiness until the listener answers once. Raise `failureThreshold` if your
Mongo is slow to accept connections; it is the only bound on boot time.

## Configuration

| Key | Env | Default | Effect |
|---|---|---|---|
| `config.server.shutdownDelay` | `ITERION_SHUTDOWN_DELAY` | `5s` cloud / `0` local | Lame-duck window before teardown. `0s` disables it — an **empty** chart value does not: it just omits the variable and the binary's own default applies. |
| `config.server.shutdownTeardown` | `ITERION_SHUTDOWN_TEARDOWN` | `30s` cloud / `60s` local | Budget for the run drain + in-flight HTTP requests, after the window. Must be > 0: zero yields an already-expired shutdown context, so in-flight runs never flip to `failed_resumable` and every request is cut. Refused at startup on every surface. |
| `probes.startup.enabled` | — | `true` | Suspends liveness/readiness during boot. |
| `probes.startup.failureThreshold` | — | `30` | × `periodSeconds` (5) = the boot budget. |
| `probes.preStopDelaySeconds` | — | `0` | Extra pre-SIGTERM sleep, for ingresses that bypass endpoints. |
| `server.terminationGracePeriodSeconds` | — | `60` | Hard bound before SIGKILL. |
| `config.runner.drainMode` | `ITERION_RUNNER_DRAIN_MODE` | `complete` | `complete` finishes the in-flight run; `interrupt` checkpoints it. |
| `config.runner.drainTimeout` | `ITERION_RUNNER_DRAIN_TIMEOUT` | `8h` | Lame-duck ceiling for a run. |
| `config.rollout.runnerEpoch` | `ITERION_RUNNER_EPOCH` | `0` | Monotonic generation, rendered literally into both PodTemplates. |
| `config.rollout.epochMismatchDelay` | `ITERION_RUNNER_EPOCH_MISMATCH_DELAY` | `2m` | Delayed redelivery while a newer runner fleet becomes ready. |

Both variables are honoured by `iterion server` in either mode and by
`iterion studio`. The **defaults** differ, though, because `iterion server`
without `ITERION_MODE=cloud` routes to the studio: locally the delay is
**0** (Ctrl-C stops now) and the teardown is 60s; only cloud mode gets the
5s / 30s pair. A malformed value (`5` instead of `5s`) is a startup error
on every surface, not a silent fallback to zero.

## Diagnosing

**502s during a deploy.** Confirm the window is actually configured:

```sh
kubectl -n <ns> exec deploy/iterion -- env | grep ITERION_SHUTDOWN_DELAY
kubectl -n <ns> logs deploy/iterion | grep 'server: draining'
```

The log line `server: draining — /readyz now 503, waiting 5s for endpoint
removal` is emitted once per shutdown. No line ⇒ the delay is 0 or the
binary predates it.

**Pod CrashLoops at boot.** `kubectl describe pod` shows whether the
*startup* or the *liveness* probe failed. A liveness kill during boot means
the startup probe is disabled or its threshold is too low.

**Runner Ready but the queue is not moving.** Ask the pod:

```sh
kubectl -n <ns> exec deploy/iterion-runner -- curl -sf localhost:9090/readyz
kubectl -n <ns> exec deploy/iterion-runner -- curl -sf localhost:9090/healthz
```

(The image ships `curl`, not `wget` — a custom `runner.image` may ship
neither, in which case `kubectl port-forward` and probe from your machine.)

`{"status":"stalled","idle_for":"7m12s"}` means the consume loop is wedged
and the liveness probe is about to restart the pod. `{"status":"draining",
"busy":true}` means it is finishing a run and will not take new work.

**A run keeps resuming after every deploy.** That is the runner's drain
ceiling, not a probe: see `config.runner.drainMode` above and
[docs/resume.md](resume.md).

## Tests

- [pkg/server/health_test.go](../pkg/server/health_test.go) — the
  critical/degraded split, the draining flip, the budget split, and the
  concurrency of the dependency pings.
- [e2e/cli_server_boot_test.go](../e2e/cli_server_boot_test.go) —
  `assertLameDuck` drives the real binary: after SIGTERM and before exit,
  `/readyz` says draining while `/healthz` still answers 200 on the same
  open listener.
- [pkg/runner/health_test.go](../pkg/runner/health_test.go) — busy vs
  wedged, and the pre-`Set` "starting" state.
