# arbitrate (Themis) ⚖️

Judges the divergence cases a modernisation programme leaves blocked, by
applying the target repository's **written arbitration doctrine** — and
nothing else. One adversarial judge, one mechanical consignment.

## Why a separate bot

The party that benefits from "accepted" must not hold the pen that accepts:
inside the delivery loop, accepting always advances and refusing always
blocks, so an embedded arbiter drifts toward yes. This bot runs as its own
session, against a doctrine it cannot edit.

## The contract it enforces

- **Refuse by default.** A case is decided only when one doctrine class
  covers it exactly, every required proof present in a committed artifact.
  Anything else — including anything that engages the *meaning* of the
  programme — escalates to a human. Escalation is a verdict, not a failure.
- **The judge decides, it never executes.** Canonicalisation stays the net
  owner's act; re-recording goes through the re-baseline ledger's rite; a
  defect goes to a lot. The judge is read-only: the consignment step refuses
  the whole run if the tree carries any change beyond its own journal write.
- **Every decision is consigned**: a machine block in the doctrine's journal
  (id, case, decision, doctrine class, cited evidence, author, timestamp),
  committed. A `rebaseline` verdict must carry the **measured** reference
  paths; unbounded re-records are refused mechanically.
- **Delegation budget** per lot (default 2, counted over the journal):
  past it, further verdicts force-escalate — a series of arbitrations on
  one lot is a signal about the lot.

## What it needs from the target repository

- `.modernize/plan.yaml` — the programme contract (blocked lots + reports).
- `.modernize/ARBITRAGE.md` — the doctrine: classes, required proofs, and
  the journal. **Written by the contract owner; absent doctrine = the bot
  refuses to judge.**

The accepted decisions still get transcribed into the plan (flags,
bookmarks) by the contract owner — this judge never holds that pen.
