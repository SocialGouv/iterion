---
name: adversarial-review-loop
description: >
  Break your own change before a reviewer does. An adversarial review loop
  whose subagents try to REFUTE the diff rather than bless it, with a round
  budget, fixes applied at the class rather than the site, mutation-proved
  tests, and named exits from a loop that has stopped converging. Use before
  pushing a branch or opening a PR, while delivering a feature, when a review
  gate keeps returning findings on the same change, or on any request to
  review adversarially, attack, break or harden a change.
---

# The adversarial review loop

A loop that converges toward a change nobody can break, run by the person
making the change, *before* anyone else reviews it.

Every rule below was paid for by a wasted round. The measured evidence is in
the appendix, with dates; the numbers come from ~250 logged rounds on Go, TS
and Python repositories.

## What it is, and what it is not

It is **not** a code-review checklist, and not a second opinion. It is a
subagent whose job is to **refute** the change, followed by a verification
pass that judges the subagent as harshly as it judged the code.

Two properties make it work, and dropping either makes it theatre:

- **The posture is adversarial.** An agent asked to "review" reports naming
  conventions. An agent asked to *break* the change reports the failure.
- **The reviewer of the reviewer is you.** Its findings are hypotheses and its
  fixes are worse than its findings (measured: a proposed fix is refuted
  ~2.5× more often than a finding). Nothing it says is applied unverified.

## The vocabulary

| Verb | Meaning |
|---|---|
| `::rva` | **One** adversarial round on a scope. |
| `::loop N` | Chain rounds toward the asymptote; `N` is the **ceiling** of local rounds authorized, not a target. |
| `::go` | Extend an exhausted budget by 4 more rounds, up to the ceiling. |
| `::rvp` | Adversarial review of the **plan**, upstream, by a model of another family. |
| `::retro` | Retrospective over the loop's own journal — judge the experiments, attack the protocol. |

These are shorthands, not a requirement: "run one adversarial round on this
diff" is the same instruction.

## 1. Scope the round, and say the budget out loud

Default scope is the change itself — everything it touches, committed or not:

```sh
# origin/main = the FETCHED branch you will merge into. Substitute yours.
root=$(git rev-parse --show-toplevel) &&
base=$(git merge-base origin/main HEAD) &&
{ git -C "$root" diff --name-only "$base"            # committed + staged + unstaged
  git -C "$root" ls-files --others --exclude-standard # and the files not yet added
} | sort -u
```

Two details, both of which produced a confidently wrong number before they
were fixed, and neither of which announces itself:

- **Chain with `&&`, never let the pipeline run on an empty `base`.** In a repo
  whose default branch is `master`, or with no `origin`, `merge-base` fails on
  *stderr*, `base` is empty, `git diff` fails on *stderr* too — and the `wc -l`
  you were told to pipe into still prints a small, plausible number counted
  from the untracked files alone. Measured: a 200-file change announcing "1
  file → ~3 rounds". Don't reach for `origin/HEAD` to derive the branch either:
  it is unset in many clones, including the one this was written in.
- **Anchor both commands at the repository root.** `git diff` is root-relative
  whatever your cwd, but `git ls-files --others` is **cwd-scoped**: run from a
  package subdirectory it silently drops every untracked file outside it. Run
  from `pkg/`, the recipe that wrote this page lost the new skill — the entire
  subject of the change.

Anything else — a module, a file, the whole repo — is a deliberate widening,
stated in the round's recap.

**Announce the budget from that list at round 1, never as a silent default:**
pipe it to `wc -l`.

Three spellings that look right and are not — all three measured on one real
change of **14** files:

- `git diff …@{upstream}…` — *fatal* on a branch with no upstream, which is
  every fresh branch and every worktree branch;
- `git diff --name-only main...HEAD` → **1554**, because the local `main` was
  seven days stale. Use `origin/main`, and fetch it;
- `git diff --name-only origin/main...HEAD` → **0**: the three-dot form
  compares two *commits*, so an uncommitted change is invisible to it. Two dots
  from the merge base, as above.

And `git diff` **never** lists untracked files — on that change the two files
it could not see included the whole point of the change.

| Changed files at round 1 | Opening estimate | Ceiling of **local** rounds |
|---|---|---|
| ≤ 8 | ~3 | 5 |
| 9–25 | ~6 | 20 |
| > 25 | ~8 | 50 |

The bands are read on the file count alone, so exactly one row ever matches.
One adjustment, and only one: **blocking code in a small diff — a hook, a lint,
a guard, a filter — raises the first row's ceiling from 5 to 10.** In anything
that blocks, a false positive breaks as much as a hole and is harder to see, so
a contained diff full of guards deserves the rounds its file count would deny
it. The wider bands already have the room.

Two numbers, two jobs, and neither is a schedule.

**The estimate is an opening prediction, not a rule.** How many rounds a change
actually needs depends on much more than its file count — how blocking the code
is, how new the surface is, how much of it the loop itself has just rewritten.
Announcing one makes the cost visible and gives the recap something it can be
wrong about; being wrong about it is normal.

**What decides the next round is the findings, not the counter.** As long as
rounds keep returning high or critical findings that survive verification,
another round is worth its cost — the estimate was simply low, and stopping
there would ship what the next round would have caught. The **ceiling** is the
cost bound: how many local rounds you may spend before the change becomes
someone else's problem.

Four rules keep the ceiling honest at both ends:

- **It counts LOCAL rounds only.** A verdict from an external gate is logged
  like a round but never budgeted.
- **Passing the estimate closes nothing, and neither does reaching it.** Say
  where things stand, what is left, and extend (`::go`, +4) toward the ceiling.
- **The stop criterion never moves**: no unassumed high or critical finding
  left — and where a gate exists, it is the gate's verdict that closes.
- **The ceiling never authorizes a non-converging loop** (§ 7). High findings
  that keep landing on your own previous fixes are not the loop working; "one
  more round" is not one of the three exits.

**Why the ceiling is generous.** A local round is paid by whoever is coding; a
gate cycle is paid by the shared reviewer credential and by everyone else's
queue time. Spending 20 local rounds to save 10 gate cycles is a good trade for
every other contributor. Measured on one long branch: 6 local rounds announced
and held, against 20 gate cycles paid.

**Scope-to-agent ratio.** A wide scope means **several subagents launched in
parallel in one message, one per surface** — a single agent over a large scope
skims. A single agent is right only when the round is bounded to code the loop
itself just rewrote: a terminal verification round (the previous round's diff
alone), or a **consolidation round** (all the fixes re-read as ONE diff — its
own target being the interactions *between* fixes, which no isolated diff
shows).

## 2. The subagent prompt carries seven things. None is optional.

1. **The posture.** "You are trying to BREAK and refute this, not to validate
   it. A report saying 'this looks solid', with no executed proof, is worth
   nothing."
2. **What the code does and which failure mode counts.** Data leak? Crash?
   False positive? Corruption? Without this it reports style and naming.
3. **The obligation to prove by EXECUTING** — command, input, observed output.
   A theoretical finding does not count. If it claims a bypass, it must first
   show the system actually does what it claims to do.
4. **The already-fixed list and the assumed list** — decisions taken,
   deliberate limits, non-goals. Without it half the round re-treads settled
   ground and the report is unreadable.
5. **The output format** — severity (critical / high / medium / low), proof,
   **minimal** fix, worst first.
6. **"If you find nothing high or critical, say so explicitly."** It is the
   only thing that makes a round's emptiness *readable*: an empty report is not
   a signal, a sentence is. (It ends the ROUND. What ends the LOOP is § 8.)
7. **"Return a partial report early"** and **"modify no file in the repo; your
   scripts go in a temp directory."** An agent that explores silently for a
   long time can die having returned nothing — and the fixing is yours, not
   its.

## 3. On return: verify the finding AND its proposed fix, before touching code

**The proposed fix is refuted more often than the finding.** Measured across
233 rounds: **85 refuted fixes for 34 refuted findings** — hardening that
starts refusing legitimate input, a ceiling that closes the service, a
predicate widened past its subject. A true finding plus a wrong fix, shipped,
is a real defect signed by your hand.

The same standard covers anything that decides a push: a finding, a fix, a
question from a gate, or **your own memory of a guard**. Two gate cycles have
been lost to a question dequeued without its two-minute test, and to a guard
asserted from memory.

**Every sentence a fix writes** — an invariant comment, a godoc, a commit
message, a doc line, a written defence in a reply to a reviewer — is an
assertion to execute in the same round. Otherwise it is the next round's
finding.

## 4. Two blocking conditions before a finding counts as handled

**(a) The class, not the site.** A finding is not handled until the `grep` for
its class has been **rendered** — the count and the verdict go in the round
recap, at authoring time as much as at fixing time.

The class is an **inventory, not a regex**: enumerate it from the *type*, or
from a round trip of the value — never by source substring (that method missed
17 field names out of 19 on one measured lot). It includes:

- the **call sites** of the touched seam;
- the **structural peers** of the fixed site;
- the **twins of the fixed path** — other I/O, sibling outputs, failure
  branches: a fix that bounds, folds, distinguishes, authorizes, waits or
  mirrors creates its own class;
- the **constructors** of the touched value when the fix changes how it is
  built, and its **readers** when the fix changes what a message, an event or
  a payload *carries* (one lot returned 4 verdicts, 3 of them consecutive, on
  readers nobody had grepped);
- the **doctrine already written** elsewhere in the repo;
- the **pre-existing uses** of any primitive the fix introduces.

Two corollaries that cost real time when forgotten: **the best fix is usually
ONE choke point**, not N repeated guards — look for the place every path
crosses; and **a shared helper beats a copied guard**, because if two places
build the same thing, the third will drift.

When the fix *is* a choke point, it is proved on the **complete enumeration**
of what it claims to cover, each case listed and executed, never on the
reported site alone. A guard proved on one case and covering the rest "by
construction" has been bypassed on later rounds by other cases from that same
enumeration.

**(b) Does the mutation go red?** Put the defect back, require red **on that
test's own assertion**, restore. A test nobody has seen fail proves nothing.
Three refinements, each paid for:

- **Mutate in the production path, never in the double.** If the oracle does
  not honour the channel under test (a filesystem store that ignores the
  context cannot observe a deadline), the bound is decorative even though the
  test is red.
- **Mutating toward EMPTY only proves presence.** To prove a choice, mutate
  toward the forbidden alternative, with the fixture arranged so that
  alternative would be *wrong*.
- **A mutation that breaks compilation falsifies nothing.** One fixture per
  branch; each mutation must redden its own test and only its own.

## 5. Re-attack the fix's diff before it merges

One agent, bounded to the diff of the fix itself, pushed to a gate or not. It
earns its cost on fixes that changed a **reading** of a shared signal rather
than a value: one lot returned 2 criticals and 3 highs in two local rounds on
code where 7 consecutive gate verdicts had returned 0 critical and 0 high.

The local round is where criticals and highs die. It does **not reliably** empty
the gate's queue of mediums: round after round, the gate keeps returning
mediums the local rounds never saw. The two find **overlapping but distinct** classes,
and neither replaces the other — a sterile local round is a reason to push, not
a reason to expect a silent gate.

## 6. Feed the next round

Move handled findings into "already fixed" and argued refusals into "assumed".
That list is the input to the next round's prompt — keeping it current **is**
the work.

A refusal parked in "assumed" that rests on a technical fact (out of scope,
load-bearing, impossible in production) carries its **executed proof**;
otherwise it is a debt, and the next round refutes it.

**One of the most frequent round-N+1 findings is a regression from round N.**
Start each round by re-reading what the previous one rewrote, and tell the
subagent: "absolute priority, these functions were just rewritten."

## 7. Non-convergence has three exits, and "one more round" is not one

**Two rounds in a row whose high findings land on the previous round's fixes
means the loop is auditing its own output.** Measured four times: 8 rounds of
false positives from the same guard, "each round found one more spelling";
8 rounds at a FLAT severity profile (2·2·2·1·2·2·3·2); 5 then 7 successive
bypasses of the previous round's defence. **A healthy loop decreases in
severity.**

Read the signal as a *proportion*, not a count: on one lot, 67 % of the gate's
findings were regressions of earlier fixes, against 16 % locally.

The three exits:

- **Requalify the scope** — a file that contains its own guards attacks itself.
- **Change the approach** rather than add an Nth patch.
- **Switch to a test of the GUARANTEE** — run the whole product under the
  hostile conditions the guard claims to neutralise and require the verdict —
  instead of enumerating the spellings the guard must recognise.

That last exit deserves its own warning. **A guard that ENUMERATES forbidden
spellings never converges**: the adversary is arbitrary text, so each round
widens the pattern and the next finds one more way to write it. A guard meant
to forbid swallowing a refusal recognised the name `Refusal`; the refusal
subclassed `SystemExit`, so `except SystemExit`, `except BaseException` and a
bare `except:` all passed. Widened, it then let through an alias, a tuple
unpacking, an `import … as`, and a subclass. The exit is not a wider pattern —
it is a different test.

Two demands come with that switch:

- **Check the bench can BITE.** One "hostile environment" bench was inert — the
  setting it targeted had no effect on the objects under test. It passed while
  proving nothing. A bench that cannot go red is not a bench.
- **Stack the layers in order**: the refusal carries the diagnosis, a check
  outlives the refusal, a floor outlives the check. Verify each by deleting the
  one above it.

## 8. Exiting the loop

**Where a review gate exists, its verdict closes the loop — not the round that
declares it done.** A sterile local round is the signal that it is time to
push, never a stop criterion: rounds a "sterile" criterion called finished have
been followed by real findings from the reviewer. The criterion counted rounds;
the gate counted defects.

So: push, then wait for the verdict. It is logged like a round and its findings
follow the same rules (class, mutation, verified fix). **Its questions** — a
non-blocking falsifiability channel, where a gate offers one — **are findings
at a lower prior**: each is *executed* (≤ 5 min) before any decision, and gets
a fix in code or in docs, or an argued refusal in the recap. Never silence. On
one PR, 2 of the 34 such questions its verdicts raised were real defects in the
code, found by execution
alone.

Where no reviewer exists, a local round over the whole change stands in for the
gate, and the recap says so.

**Say what the loop cost, in the commit.** Two trailers, so the claim is
greppable and falsifiable rather than implied:

```
Adversarial-Rounds: 3 local (announced 3, ceiling 5)
Adversarial-Model: claude-opus-5[1m]
```

A change reviewed by nobody writes `Adversarial-Rounds: 0 (trivial: typo)` —
explicitly. An absent trailer is indistinguishable from an oversight, which is
exactly the ambiguity the signal exists to remove.

## Upstream: `::rvp` — attack the PLAN, by another model family

`::rva` attacks the result; `::rvp` attacks the plan, before a line is written.
On a substantial subject the two compose: **`::rvp` → implement → `::loop`**.

- **Another family.** The reviewer is a model from a different family than the
  author's (a GPT-class model reviewing a Claude-authored plan, or the
  reverse). A model reviewing its own family's plan agrees with it.
- **Highest reasoning effort the harness exposes**, on the plan alone.
- **The feedback is integrated with an explicit disposition** written into the
  plan: adopted / adjusted / dismissed, each with its reason. A dismissal with
  no reason is the next round's finding.
- Worth it when the plan is multi-file, a migration, or a new surface. A small
  plan skips it.

Where a plan review cannot be had (one provider credentialed), the plan
proceeds **loudly** — stamped as unreviewed — never silently.

## Keeping the loop honest: journal, experiments, retro

The protocol above is not doctrine; it is the surviving subset of what was
measured. Keeping that true requires three cheap habits.

**A journal.** One JSONL line per round, appended at the end of every round
with no gaps. It needs an address or it will not exist: pick one and write it
down — a per-repo `.rva/journal.jsonl` (gitignored, or committed if the team
wants the history shared), or one file per operator across all repositories,
with `repo` telling the lines apart. The choice matters less than naming it;
an unnamed journal is a discipline nobody can audit, and it is what a round
count in a commit message is checked against.

```json
{"ts":"…","repo":"…","cmd":"rva|loop","scope":"…","round":1,
 "budget_announced":3,"diff_files":6,"agents":2,"consolidation":false,
 "findings":{"crit":0,"high":1,"med":2,"low":3},"confirmed":4,
 "fixes_applied_prod":2,"refuted_findings":1,"refuted_fixes":2,
 "regressions_prev":0,"budget_exhausted":false,"variants":[],"lesson":null}
```

Three field rules that each cost a retrospective to learn: `round` orders the
rounds of a loop, **never `ts`** (lines written after the fact, and backfilled
gate verdicts, carry out-of-order timestamps); `confirmed` is **per round**,
never cumulative; `repo` is the short name, consistently (the drift came back
within five days of being fixed).

**Pre-registered experiments.** A variation on the protocol is written down
*before* it runs, with its metric and the direction expected. A metric chosen
afterwards does not count. Cap the active set (three is workable): promoting a
fourth means retiring one.

**A retrospective**, every ~5 loops:

1. Judge each active experiment against **its own pre-declared metric**, and
   nothing else. Confirmed (≥ 3 runs, ≥ 2 contexts, signal in the predicted
   direction) → promote. Contradicted → graveyard, with the reason. In
   progress → say how many runs are missing.
2. **Attack the protocol itself** — mandatory, not optional. For each rule:
   does the journal contain runs that contradict it (rule followed AND round
   wasted, or rule ignored with no damage)? A retrospective that only *adds* is
   suspect. At every promotion, look for what to **evict**: a protocol nobody
   can hold in their head stops being followed.
3. Seed at most 2–3 new hypotheses, each with its metric written first.

Four anti-bias guards: **small-n humility** (these are signals, not proofs;
compare comparable runs); **pre-registration**; **look for the counter-evidence**
(list the runs that contradict a hypothesis, not only the ones that flatter
it); **a permanent graveyard** (a disproved idea does not come back "just to
see"). A single spectacular run promotes nothing — at best it seeds a
hypothesis.

**The loop's output is not only corrected code.** Whatever was paid for twice
becomes a written rule or a canary test that refuses the next occurrence.
Otherwise the next project pays for it again.

And never legislate mid-run: a run feeds the journal, it does not amend the
protocol.

## What makes the loop useless

- Fixing without having reproduced — you are coding against a false hypothesis.
- Not passing on the "assumed" list — the next round re-reports the same
  findings.
- Treating a false positive as less serious than a hole: in anything that
  **blocks** (a hook, a guard, a lint, a filter) a false positive breaks as
  much as a vulnerability, and it is harder to see.
- Concluding on an empty report without having demanded the sentence "nothing
  high or critical": a silent agent is not an agent that found nothing.
- Fixing an inconvenient test in the direction that suits you: when an
  expectation is wrong, correct it toward the **real** behaviour, never toward
  green.

## Short recap every round

Handled / dismissed (with the reason) / remaining. When the budget is spent,
**do not conclude that it is finished**: say where things stand, what is left,
and ask.

## Appendix — the measured evidence

From the journal of the loops that produced this protocol (Go, TypeScript and
Python repositories, 2026).

| Claim | Measurement |
|---|---|
| The local round is cheaper than the gate | A fresh line pushed straight to the gate: **5 consecutive verdicts at ≥ 1 medium, ~6 h of queue**. The same shape after one **15-minute local round**: first verdict at **0 findings**. |
| The fix is worse than the finding | **233 rounds: 85 refuted fixes, 34 refuted findings** (~2.5×). An external gate over **81 verdicts / 109 findings**: **0** wrong findings, **11** wrong fixes. |
| Local and gate find disjoint classes | One lot: **2 criticals + 3 highs in 2 local rounds**, on code where **7 consecutive gate verdicts** returned 0 critical, 0 high. |
| A sterile local round is not a stop criterion | **0 occurrences in 66 rounds** of the criterion actually holding; and 8 logged gate verdicts found **4 classes** the internal rounds had skirted — including a HIGH *after* a closure had been declared. |
| Non-convergence is visible as a proportion | Regressions among findings: **67 % gate-side vs 16 % locally** on the same lot. |
| A round count cannot be predicted from a scope | Real convergence: **1, 3, 4, 5, 5, 6, 8** local rounds. Over 20 seeded loops the distribution is bimodal (2–3 / 8+), median 8; **75 % exceed 4 rounds**, exactly one converges at 4. Two loops in the widest band refused the band's own number and were right both times. |
| Rounds vs gate cycles, on one long branch | **6 local rounds** announced and held, **20 gate cycles** paid. |
| The class is an inventory, not a regex | Source-substring enumeration missed **17 field names out of 19**. |
| Questions are findings at a lower prior | **2 of the 34** non-blocking questions raised by one PR's 20 verdicts were real code defects, found by executing them. |
