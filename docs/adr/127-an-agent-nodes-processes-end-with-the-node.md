# ADR-127 — An agent node's processes end with the node

- **Status**: Accepted · **amended 2026-10-21** (see [Amendment](#amendment-2026-10-21--a-sandboxed-tool-node-proves-its-end))
- **Date**: 2026-10-14
- **Applies to**: every agent node, whatever its backend — the delegated CLIs
  (`claude_code` and its formatting pass, `codex`, `cliagent` and the backends
  built on it, `pi` over RPC and in print mode) through `pkg/backend/delegate`;
  the claw tool loop's shell through `pkg/backend/tool`, and a sandboxed claw
  node's in-container runner; the sweep itself in `pkg/internal/proc`; the
  headless rule the engine injects into `claude_code` sessions;
  `docs/backends.md`
- **Neighbours**: [ADR-082](082-sandbox-by-default.md) (one container per run,
  shared by every node), [ADR-125](125-a-lots-contract-is-a-directory-judged-on-what-lands.md)
  (a verdict judged on what lands, not on what the judged party left behind)

## Context

The engine tells a headless agent, in the rule it injects into every
`claude_code` session, to "start anything longer with `nohup … &` and poll
it". Agents do. Nothing stops what they start.

At the end of a delegation the engine terminates the delegated CLI and nothing
else:

- in a sandbox, `killSandboxDelegate` sends TERM, then KILL, to the one PID the
  spawn wrapper recorded — the CLI's own;
- on the host, the SDK closes the CLI's process group on an abort, but returns
  without a signal when the CLI exits cleanly;
- the claw shell runs each command in its own group, killed on timeout or
  cancel only.

A server started with `nohup … &`, a daemonised database, a process that
called `setsid` — all outlive the node. In a sandbox they live until the run's
container is removed, next to every node that runs after.

Measured on a modernization campaign, one lot's run:

| | |
|---|---|
| the agent node | started the target application's world — application server, database, object store — through the target's own scripts; ran the behavioural oracle against it in gate mode: every mutant detected, collateral 0; ended its work without one stop command |
| one minute later, the gate node | booted its own world in the same workspace. The target derives its state directory and its ports from the workspace path, so the gate's world and the agent's leftovers shared both. The gate's capture refused: a persona could not log in (HTTP 500) |
| the same commit, on a fresh world | booted by the same `up` sequence: the persona logged in (302 to its landing page, then 200); a wrong password was refused; the application log held no error |

The lot was refused for a defect it did not have, after seven hours of work.

The refusal is the benign outcome. A target script that recognises a running
application by its own pidfile does not see an application it did not start,
and a readiness probe that accepts any status below 500 is satisfied by a stale
server: a gate can then judge an application built from another tree — a green
verdict on a world the gate did not boot.

## Decision

**Every process an agent node starts ends with the node.**

1. **A session per delegation.** When a node spawns its agent, the engine
   draws a random 128-bit token and exports a variable of its own,
   `ITERION_NODE_SESSION_<hash>=1`, in the agent's environment — the hash of
   the token in hex, which cannot spell the words (KEY, TOKEN, SECRET) on which
   an agent's own sandbox drops a variable from the commands it runs. Every
   descendant inherits it — `nohup`, `&`, `setsid`, a double fork, a server's
   workers. A delegation nested in another adds its own variable next to the
   outer one, so its processes belong to both. Retries inside one delegation
   keep the session, so the end of the delegation reaches every attempt. The
   claw loop has no process of its own to export it from: its shell and its
   settings hooks add it to what they start. A sandboxed claw node's runner is
   spawned inside the session and draws none of its own — the launcher tells
   it so in the task — so that the launcher's sweep, which records the node's
   events, is the one that stops and reports what the node left.
2. **A sweep on every exit path.** When the delegation ends — clean, failed,
   aborted or panicking — the engine stops the session's processes. A scan
   finds them three ways:
   - the processes whose environment holds the exact entry
     (`/proc/<pid>/environ`, read through one of its threads when the leader
     thread has exited and taken the process-wide file with it);
   - the processes an earlier scan found, as long as they are the same process
     — the pid and the start time both match;
   - every descendant of either, by parent: a worker that rewrote its title
     over its environment, as a database backend does, or a child started with
     a cleared one, stays in the session through its parent.

   The sweep sends TERM, once, to each process a scan finds, and scans again
   every 50 ms — new processes get their TERM too — until two scans in a row
   find nothing, or until a grace of five seconds is over; then KILL the same
   way, for up to three seconds, unless the TERM phase ended on two empty scans.
   One empty scan proves nothing: a process in the middle of an exec has no
   readable environment for that instant. Every round signals what its scan
   found before it looks at the clock, and every signal goes to a pid checked
   right before to still be the process the scan found.
   - In a sandbox the scan runs inside the run's container, through one short
     exec — the channel that already kills the CLI — as a POSIX sh program
     that reads environments with `grep -z`, or with `tr` where grep cannot
     read NUL-separated records. It runs before the CLI's own kill: a CLI
     still running at the end — its node aborted, its client killed — is a
     carrier, and what it started is found as its descendant before the
     CLI's end orphans it. The kill of the recorded CLI stays, for a sweep
     that could not run. The pid the spawn recorded sits where any process of
     the agent's uid can rewrite it, so it is trusted only as a pid above 1:
     never a process group, never the container's init. Its last line says it finished: a program that died
     on its way is reported as an error, never as a sweep that found
     nothing.
   - On the host, and for the claw loop wherever it runs, the engine scans its
     own `/proc`.
   - Where there is no `/proc`, the sweep cannot run, and the engine says so in
     a warning. It is never skipped silently.
3. **What a sweep could not do is said, never guessed.** A process still
   running after the KILL is named as an error. A process of the same user,
   started since the session began, whose environment no one can read — a
   program that made itself non-dumpable — may carry the session: it is named,
   and left running. The sweep kills on evidence only.
4. **What was stopped is said.** Each sweep that stopped something leaves a
   warning in the run log naming the processes, and a `delegate_background`
   event, phase `abandoned`, that names them — on every backend, routers
   included; so do a survivor of the KILL and a sweep that could not look or
   did not finish. The CLI the sweep stopped with its session is the spawn,
   not something it left, and is not named. A process whose environment no
   one can read may be anyone's of this uid, started while the node ran: the
   run log names it, the node's events do not. On
   `claude_code` the names also join the background work the delegation lost
   (`TerminatedBackgroundTasks`), so a run forked from that turn tells the
   resumed agent its server is gone instead of letting it poll a dead port.
5. **An agent's exit is the end of its output.** A CLI agent's output is not
   waited for more than two seconds past the agent's exit: a process it left
   holding the output pipe cannot hold the node, and the sweep then stops it.
6. **The injected rule says it.** Background work is still started with
   `nohup … &` and polled; it ends with the node, and the nodes after it start
   their own.

There is no opt-out. No bot in the catalogue relies on a process an agent
started surviving into a later node; a bot that needs a long-lived service
across nodes needs a declared mechanism with its own lifetime, reviewed as
such — not a leak that happens to work.

## Alternatives considered

- **A strike node before each gate**, in the bots that gate an agent's work
  (`modernize`, the golden-master rites, their subbots). It repeats the guard
  in every bot that has the agent-then-gate shape, and needs its own baseline
  to spare what was running before. On a host run it cannot tell the agent's
  processes from the operator's, so it would have to refuse — leaving the
  defect there.
- **The oracle harness runs the target's `down` before its `up`.** It stops
  only what the target's `down` knows — an application, not necessarily its
  database. A gate's own commands still run against the stale world. The
  harness belongs to each net and is re-synced per target, so the fix would
  reach a campaign late.
- **Kill the CLI's process group at node end.** A process that called
  `setsid`, or was reparented to the container's PID 1 once the CLI exited,
  is no longer in the group.
- **Snapshot the process table before the node and kill the difference.** In
  a shared container, concurrent branches and the runner's own execs —
  workspace checkpoints — appear in the difference.

## Consequences

- A node after an agent node judges the world it booted, as far as the
  agent's processes are concerned — whatever the target, and on every backend.
- A daemon the node started is stopped with it, even when something else came
  to use it. Two branches of a fan-out that share the run's container and one
  build daemon lose it when the branch that started it ends; the other
  branch's next build starts its own. A daemon that was running before the
  node, and that the node only connected to, is not of the session and is
  spared.
- A paused node ends its processes at the pause: the resumed node starts its
  own, as it would on a fresh runner.
- Residuals, each documented where it applies:
  - a process that detaches from every process of the session and then loses
    the variable leaves no trace the sweep can follow, and outlives the node:
    a daemon that unsets it before its double fork, and — by default — one
    that daemonizes and then rewrites its title over its environment, as
    `nginx` and `redis-server --daemonize yes` do — and a child started with
    a cleared environment whose parent is gone before the sweep looks: the
    CLI of an earlier attempt, ended by a retry, or a process that exited on
    its own. Closing it needs the delegate's spawn to become a subreaper, so
    that what detaches or is orphaned is reparented to it rather than to
    PID 1;
  - what a container engine starts — `docker compose up -d`, `podman run -d`
    — runs under the engine's daemon: neither a descendant of the node nor a
    carrier of its variable, it outlives the node. A node that boots its world
    through a container engine stops that world itself, and a bot that needs
    such a world across nodes declares its lifetime; the session reaching it
    would take the engine's own labels;
  - the claw `repl` tool starts its interpreters through claw-code-go, which
    does not take the engine's environment additions: what a `repl` call
    leaves running outlives the node until claw-code-go accepts them;
  - a process whose environment cannot be read is reported, not stopped —
    and in the run log only: a daemon of the node that makes itself
    non-dumpable, as `ssh-agent` does, cannot be told from another process
    of the uid, and outlives the node with a line in the log.
- A tool node's own leftovers are out of scope: a deterministic node that
  backgrounds work and exits 0 is legitimate, and its group is killed on
  cancel only. In a sandbox this held only from the amendment on: see
  [Amendment (2026-10-21)](#amendment-2026-10-21--a-sandboxed-tool-node-proves-its-end).
- Cost: in a sandbox, one exec per delegation end, next to the existing CLI
  kill; elsewhere, two `/proc` scans 50 ms apart when the node left nothing.

## Amendment (2026-10-21) — a sandboxed tool node proves its end

The consequence above — a tool node's group "is killed on cancel only" — held
on the host and not in a sandbox, and the sandbox hid a worse defect behind it.

Measured on a modernization campaign's net rite. The gate node is a tool node
whose script boots the target's world and applies mutants for two to three and
a half hours, printing its report only at the end. It finished 52 minutes in,
with an empty result and no error. Its script was still running in the pod: it
went on applying mutants to the workspace while the agent node the workflow
looped into worked there — a mutant was applied in `src/` when that agent
started; the agent noticed it and stopped the script. The same node, run again
in the same run, took 2 h 07 and reported normally.

The mechanism, in the code. In a sandbox, a tool node's verdict was the exit
status of the local exec client — `kubectl exec`, `docker exec`. client-go
reads an error stream that closed without a status as success, so a stream lost
behind the client (the API server's WebSocket-to-SPDY translator and the
kubelet leg behind it, a kubelet or container runtime restart) ends the client
with status 0 and whatever output had arrived. The engine parsed that output —
`{result: ""}` for an empty stdout — and the node succeeded. A cancel killed the
client, and nothing in the pod.

### Decision

7. **A command the sandbox runs for a tool node proves its end.** Every such
   command — a recipe, a script, a postcondition, a self-repair — is built by
   one function (`sandboxToolCommand`), which wraps it in a POSIX sh program
   (`sandbox.Completion`). A driver that runs commands on the engine's own host
   (`noop`, behind the child-devbox decorator too) hands back the process
   itself — its exit status is the command's own — and the command runs as it
   is.
   - **The command keeps what an exec'd command has.** It leads its own session
     and process group (`setsid`, where the container has it), so its own
     `kill 0` reaches its processes, not the wrapper. The wrapper turns off job
     control and errexit first — both can come from the environment, and
     either would let `setsid` fork or end the wrapper before its line — and
     outlives a HUP, INT or TERM sent to it. The session's variable is in the
     exec's own environment, so the wrapper is of the session too.
   - **The line.** Once the command has exited, the wrapper prints on stdout
     one line holding a random nonce and the command's exit status — retried
     while a stdout the command left non-blocking is full — then exits with
     that status. On stdout, after everything the command wrote there, the line
     also proves that the command's whole stdout arrived. The engine reads it
     back out before anything else sees stdout.
   - **The line found:** the command ended; the node succeeds only if the
     client and the line both say 0. A proven end sweeps nothing.
   - **The line missing:** the end is unknown. The command's session is swept
     in the container (decision 2's sweep). A sweep whose exec never reached
     the container — its client could not connect or open the stream — is
     tried again, backing off: the exec path a stream was lost on is often
     down a little longer than the stream. Any other unfinished attempt is
     final: it may have signalled processes no later scan can find again. The
     whole sweep ends within three minutes — ten seconds on a cancel — and
     two seconds past that at most, for a killed client something still holds
     the output of. The node fails with a `sandbox.ExecStatusUnknownError`
     saying what the sweep stopped and what it could not do; never a success
     carrying a partial output.
   - **An end the sweep could not settle** — its program never ran to its end
     within its budget, or something it signalled survived its KILL — leaves
     the command possibly running: the error is an ambiguous effect
     (`runtime.AmbiguousEffect`). A process of the uid no one can read is named
     and settles nothing either way. An ambiguous effect is recorded as
     `AMBIGUOUS_EFFECT` above the code a wrapper of the same process gave it
     (a subbot boundary, the catch-all default) and above a budget deadline
     that landed with it; its status write is detached from a done context —
     a cancel, a drain — so that a store does not refuse it, while any other
     failure that lands with a cancel is left to the cancel's own handling
     (the status the publisher wrote, the drain's redelivery). Recovery never
     retries it, the runner and the CLI read it as terminal, the retry sweeper
     disarms a stale armed retry on it, and a join stops on it, `best_effort`
     as `wait_all`. The residuals list where this does not hold yet.
   - **A Verified Action** stops on an ambiguous end of its recipe, of a
     self-repair or of its postcondition; a recipe whose end was unknown but
     settled lends it no output — the postcondition judges, and only its
     output travels; a cancel ends the node with its cancel, never with a
     `best_effort` success.
   - **The input proves its end too.** A recipe over 100 KB is streamed to a
     private temp file in the container, run only once its size is the size
     sent, as `bash -s` reading that file — what `bash -s` makes of it, a `read`
     in it reading its next lines included; a cut stream is refused.
     A file `WriteFileExec` writes into the sandbox (a script's body pushed
     into a copy-based sandbox, a credential) is put in place only once its
     received size is the size sent, by a writer of its own that refuses a
     directory as its destination and removes its temp file when a signal ends
     it, and which then says so on stdout — the write is a success only when it
     said so.
8. **A tool node that backgrounds work and exits 0 stays legitimate:** a
   proven end sweeps nothing, on a cancel too.

### Consequences

- A gate whose exec session is lost fails loudly, and the command it started
  is stopped — or, when it cannot be, the run stops for a human, recorded as
  such, rather than start a second copy beside it, save on the paths the
  residuals list.
- A cancelled sandboxed tool node ends its processes, as a host tool node's
  group is killed on cancel.
- Cost: one `sh` per sandboxed tool command; for an unknown end, one exec per
  sweep attempt; one line on the writer's stdout.
- Residuals:
  - a fan-out drops an undecided branch in two cases: the parent's own cancel,
    drain or deadline ends the fan-out before it reads its branches' results
    (the fan-out, the per-item fan-out, a router's `multi`), and a `wait_all`
    sibling's failure cancels a branch whose sweep outlasts the branch cancel
    grace (five seconds, where a cancelled sweep may take its ten). The run
    then records the other failure, which the runner and the CLI may resume;
  - a subbot child read back from its record — one that paused at a human
    gate, was resumed and then ended undecided — reaches its parent as plain
    text: the parent records `EXECUTION_FAILED`, and recovery may run a fresh
    child. The runner's, the CLI's and the dispatcher's subbot waits rebuild
    the child's error the same way;
  - an operator cancel in the cloud: the publisher writes `cancelled` before
    the engine sees the cancel, and the resumable failure write skips a
    cancelled run, so the run's record says `CANCELLED` while its events say
    `AMBIGUOUS_EFFECT`. Nothing resumes a cancelled run on its own; a human
    reading the record alone does not see the undecided effect;
  - the engine's other execs into the sandbox — the workspace checkpoints
    (`git add -A` and the commit), the scratch bank, the capture of the
    workspace head, the git fixups — still take the client's status for the
    command's end. Closing them takes the same completion at the drivers' exec;
  - a run whose end stayed ambiguous is torn down as any failed run: its
    workspace is exported and banked while the command may still be writing.
    A human reconciles it before reusing the bank;
  - a sweep that cannot reach a pod that is already gone cannot tell it from
    an exec path that is down: the run is parked, and a human confirms;
  - a KILL sent to the wrapper itself ends it before its line: the node fails
    on an unknown end;
  - a proven end with a non-zero status sweeps nothing: what the command left
    running before it failed runs on until the run's container goes, as a host
    tool node's leftovers do;
  - `/bin/sh` shells without a `printf` builtin that reports a full
    non-blocking pipe (mksh, OpenBSD ksh) can lose the line on such a pipe:
    the node fails on an unknown end;
  - the guard that every call into an exec seam of `pkg/backend/model` is a
    listed one reads the syntax: a method value taken through a local alias
    escapes it; the behavioural tests of the builders stand behind it.
