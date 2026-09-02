# Agent operating contract — iterion

This file is the **single source** for the cross-agent working methodology on
iterion. Every **interactive agent session** (Claude Code, Codex, pi, or any
other harness driven by an operator) follows it. **Automated bot runs**
(iterion-launched campaign/review/fixer bots executing on this repo) are out
of scope: they follow their own mission contract and MUST NOT attempt the
board rituals below — no claiming, no ticket creation; their `.bot` mission
is their ticket. The full engineering reference (architecture, build, DSL,
conventions) lives in [CLAUDE.md](CLAUDE.md) — read it before touching code;
this file only carries the work-tracking contract, so it stays cheap to inject.

## Work tracking & session methodology — the GitHub board

**The [Iterion project board](https://github.com/orgs/SocialGouv/projects/203)
is the truth for ongoing work.** Every non-trivial task — engine, bots,
cloud/ops, studio, docs — is a GitHub issue on that board, whatever workspace
it executes in (this repo, a sibling lab, a prod incident). Statuses:
Inbox → Planned → In progress → Blocked → Done; fields `Area` and `Mode`
(planned dev mode: `dogfood` vs `direct`). The iterion **native board** is NOT
replaced: it stays the bots' operational surface (auto-triage, dispatch);
the GitHub board is the roadmap/chantier view. An `iterion issue import`
mirror (GitHub → native, one-way idempotent) can bridge the two when a
ticket should be dispatched to a bot.

A work session on iterion follows three phases:

**Phase A — plan & align (start of session).** Read the board, triage the
Inbox, make the statuses true, pick or confirm the session's ticket. Work
discovered mid-session becomes a new issue, not a side quest.

**Claim before work (multi-session rule).** Several agent sessions (Claude
Code, Codex, others) often run in parallel on iterion. A session *claims*
its ticket before coding: Status → In progress + a timestamped "claimed"
comment naming the session. Never touch a ticket already claimed by another
session without the operator's arbitration. Release the claim at session
end: Done with evidence, or back to Planned with a state-of-work comment.

**Phase B — dev, mode chosen per ticket.** *Dogfood-first reflex*: before
implementing by hand, ask "can a catalog bot do this work?" — if yes,
propose launching it (visible in the operator's studio, actively monitored,
bilan in `docs/bot-runs/`), and improve the bot on every friction the run
surfaces. Propose this mode regularly; don't impose it. Otherwise *direct
dev*: a normal coding session. Either way the existing contracts apply
(Revi→Billy habit on PRs, commit scope discipline, bilans).

**Phase C — close with evidence.** The issue closes with a link to the
PR/commit/bilan that proves the work; board status updated before the
session ends. A ticket that says In progress with nobody on it is a bug
in the board — fix it when you see it.
