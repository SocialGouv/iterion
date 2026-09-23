# Agent operating contract — iterion

This file is the **single source** for the cross-agent working methodology on
iterion. Every **interactive agent session** (Claude Code, Codex, pi, or any
other harness driven by an operator) follows it. **Automated bot runs**
(iterion-launched campaign/review/fixer bots executing on this repo) are out
of scope: they follow their own mission contract and MUST NOT attempt the
board rituals below — no claiming, no ticket creation; their `.bot` mission
is their ticket. [CLAUDE.md](CLAUDE.md) is the router — project stance, the
before-merge contract, build commands — and it routes to the engineering
reference, which lives one level down in the doctrine tree at
[docs/agents/](docs/agents/README.md) (architecture, DSL, backends, bot
authoring, testing, runbooks), read on demand. Read CLAUDE.md before touching
code, and open the tree page your task names; this file only carries the
work-tracking contract, so it stays cheap to inject.

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

**Every ticket lives under an epic.** The board is organised by **`epic:`
issues** — one per chantier, carrying its own state of play, its acquis and
its open sub-issues. A ticket belongs to its epic **twice**: as a **sub-issue**
(the authority) and through the **`Epic` field** (the projection the views
need — a board column accepts a single-select, never "Parent issue"). A new
ticket gets both, and `task board:epics:sync` says so. `epic:` is an
*objective container*; a multi-page specification is a **`design:`** ticket
that an epic consumes. Mechanics, views and the one step the API cannot do:
[docs/board-epics.md](docs/board-epics.md). How proven each surface actually
is — the repo-side twin of the epics: [docs/state-of-the-art.md](docs/state-of-the-art.md).

A work session on iterion follows three phases:

**Phase A — plan & align (start of session).** Read the board, triage the
Inbox, make the statuses true, pick or confirm the session's ticket. Start
from the **🎯 Epics** view rather than the flat Kanban: it says where each
objective stands, which is what picking work needs. Work discovered
mid-session becomes a new issue **under an epic**, not a side quest.

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
(the before-merge review loop, commit scope discipline, bilans).

**Phase C — close with evidence.** The issue closes with a link to the
PR/commit/bilan that proves the work; board status updated before the
session ends. A ticket that says In progress with nobody on it is a bug
in the board — fix it when you see it.

**Before merge, the review loop is required.** A change reaches `main`
through a PR whose `revi/review` gate is green (admins, and the release
bot, may bypass), and the gate is not the first reviewer: run a **local
adversarial round on the diff before pushing** — a subagent whose posture is to break the change, with every
finding *and every fix it proposes* verified before a line is written.
The gate closes the loop; a sterile local round only means "time to
push". A feature is delivered *through* that loop — plan review upstream
by another model family, a round per slice, a round on the whole diff
before the push — not reviewed once at the end. Findings are the
developer's to fix, by hand or through another local round — **`/billy`
is paused on this repo** (cost, until the team spends its own BYOK key).
What decides another round is the findings, not a counter: while rounds
keep returning verified high/critical, keep going. The budget is a
**ceiling on cost, not a target** (≤ 8 changed files → 5 local rounds,
10 if the diff blocks — hook/lint/guard/filter · 9–25 → 20 · > 25 → 50),
because a local round is paid by your own plan and a gate cycle by the
shared credential. Every commit **that ships reviewed work** then says
what the review cost — `Adversarial-Rounds:` and `Adversarial-Model:`
trailers, `0 (trivial: …)` written explicitly rather than omitted.
Protocol:
[docs/agents/adversarial-review-loop.md](docs/agents/adversarial-review-loop.md);
gate and merge mechanics:
[docs/agents/review-and-merge.md](docs/agents/review-and-merge.md).
