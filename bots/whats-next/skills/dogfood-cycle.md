---
name: dogfood-cycle
description: >
  The operator's proven ritual for validating a bot by running it for
  real — launch on a real task, monitor actively, fix BOTH sides on
  failure, land, close the loop with a bilan. Use whenever you propose
  or oversee a live bot run.
---

# Dogfood Cycle — validate bots by real runs

A bot is validated by a **real run on a real task**, never by reading its
DSL or passing stub tests. Deep bugs (hangs, stuck nodes, silent façades,
budget spirals) only surface at execution time. When you propose or
supervise a bot run, drive this cycle:

## The cycle

1. **Launch on a real task**, with side-effects contained by flags, not by
   hiding the run: storage-branch-only merging (`--merge-into none`) when
   the work must be reviewable before landing, board writes toggled by the
   bot's var when it is a drill. The run MUST be visible to the operator's
   studio (their store dir) — a run they cannot watch does not count.
2. **Monitor actively** — monitoring is not waiting:
   - Watch node progression and logs for anomalies, not just the final
     status. An incoherent state (node "running" with no events, duration
     climbing with no tool calls, the same file re-read in a loop) is a
     finding — surface it.
   - **Cut idle spins.** If the run stops progressing (no new events, a
     loop re-discovering the same ground, an impasse), do not let it burn
     time and budget "just in case" — pause/cancel, diagnose, resume.
3. **On failure, fix BOTH sides.** A dogfood failure has two candidate
   culprits and they are BOTH in scope:
   - the **bot** (prompt, schema, graph, vars), and
   - the **engine/runtime** underneath it (a dogfood run is the best
     engine-bug detector there is).
   Attribute before patching: reproduce minimally, find the real cause,
   fix it at the source — never paper over an engine bug with a prompt
   tweak or vice versa.
4. **Re-run to prove the fix.** The cycle converges when a run completes
   with the expected artifacts (commits landed where announced, report
   written, board updated) — not when the fix "should work".

   A full re-run is not always the cheapest proof. When the failure was
   one node's configuration, `iterion rewind` replays from that node and
   keeps the upstream nodes already paid for:

   ```sh
   # after editing the bot:
   iterion rewind --run-id RUN --auto     # or --node <pivot>
   iterion resume --run-id RUN --force
   ```

   **Know what it does to the files.** On a run with no `worktree: auto`
   the workspace is the operator's LIVE CHECKOUT, and the rewind restores
   it, not just the checkpoint. It is bounded — by default only the paths
   the run is *recorded* to have changed (`--restore-scope produced`) —
   but it still writes to a tree that may hold uncommitted human work.
   Read the paths it prints; it names what it overwrote and what it left.
   `--restore-scope none` skips the file half entirely, and the way back
   is `iterion rewind --run-id RUN --list-snapshots` then
   `--restore-snapshot <id>` (full-tree, and it banks first).
5. **Land and close the loop:**
   - land the validated fixes (bot + engine) promptly;
   - move the tracking card/ticket to its rightful column;
   - write a dated **bilan** of the run (what it caught, what it missed,
     what to change next time) in the repo's bot knowledge base if it has
     one. A run whose lessons evaporate with the worktree did not finish.

## Why this shape (measured)

This mirrors how productive operator sessions actually behave: real
execution is the value engine (the deep bugs never surface from reading),
active supervision front-loads corrections, and every session closes with
a persistence artifact. See the engine repo's
`docs/references/productive-session-patterns.md` for the numbers.

## Anti-patterns

- **Fire-and-forget**: launching a run and only checking the terminal
  status. The middle of the run is where the signal is.
- **Blame-one-side**: assuming the bot is wrong and the engine is right
  (or the reverse) without attribution.
- **Invisible runs**: running into a store or worktree the operator
  cannot observe, then reporting success.
- **Endless patience**: letting a stuck run "finish" to see what happens.
  Time and budget spent on a spun-out run is signal destroyed.
- **Validation by adjacency**: "the bot compiled and the graph validates"
  is not a dogfood result.
