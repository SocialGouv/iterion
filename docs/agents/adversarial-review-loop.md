# The local adversarial review loop — break your own diff before the gate does

This is the round a developer (or an agent session) runs on their own change
**before pushing it to the merge gate**. It is not a code-review checklist and
not a second opinion: it is a subagent whose job is to **refute the change**,
followed by a verification pass that judges the subagent as harshly as it
judged the code.

**Why it is required rather than encouraged.** Measured on this repo: a fresh
line pushed straight to the gate took **five consecutive `revi/review`
verdicts at ≥ 1 medium, about six hours of merge-queue time**, against **one
15-minute local round followed by a first verdict at 0 findings**. The gate is
a slow, shared, serialized reviewer. Spending its cycles on findings a local
round would have caught costs everyone else's queue time too.

The contract this serves is in [review-and-merge.md](review-and-merge.md).

---

## 1. Scope the round, and say the budget out loud

Default scope is the change itself:

```sh
git diff @{upstream}...HEAD      # falls back to main...HEAD, then HEAD~1
git diff HEAD                    # plus the working tree when it is dirty
```

Anything else — a module, a file, the whole repo — is a deliberate widening,
stated in the round's recap.

**Announce the budget from the scope at round 1, never as a silent default:**

```sh
git diff --name-only <base>...HEAD | wc -l
```

A wide scope means **several subagents launched in parallel in one message,
one per surface** — a single agent over a large scope skims and reports naming
conventions. A single agent is right only when the round is bounded to code
the loop itself just rewrote: a terminal verification round (the previous
round's diff alone), or a **consolidation round** (all the fixes re-read as
ONE diff — its own target being the interactions *between* fixes, which no
isolated diff shows).

## 2. The subagent prompt carries seven things. None is optional.

Each of these was paid for by a wasted round.

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
6. **"If you find nothing high or critical, say so explicitly."** This is the
   loop's only stop signal: an empty report is not a signal, a sentence is.
7. **"Return a partial report early"** and **"modify no file in the repo; your
   scripts go in a temp directory."** An agent that explores silently for a
   long time can die having returned nothing — and the fixing is yours, not
   its.

## 3. On return: verify the finding AND its proposed fix, before touching code

**The proposed fix is refuted more often than the finding.** Measured across
115 rounds: **48 refuted fixes for 26 refuted findings** — hardening that
starts refusing legitimate input, a ceiling that closes the service, a
predicate widened past its subject. A true finding plus a wrong fix, shipped,
is a real defect signed by your hand.

**Every sentence a fix writes** — an invariant comment, a godoc, a commit
message, a doc line — is an assertion to execute in the same round. Otherwise
it is the next round's finding.

## 4. Two blocking conditions before a finding counts as handled

Each has cost whole rounds, repeatedly.

**(a) The class, not the site.** A finding is not handled until the `grep` for
its class has been **rendered** — the count and the verdict go in the round
recap, at authoring time as much as at fixing time (see the repo rule on
fixing at the class). The class is an inventory, not a regex: the **call
sites** of the touched seam, the **structural peers** of the fixed site, the
**twins of the fixed path** (other I/O, sibling outputs, failure branches — a
fix that bounds, folds or distinguishes creates its own class), the
**constructors** of the touched value (not its readers), the **doctrine
already written** elsewhere in the repo, and the **pre-existing uses** of any
primitive the fix introduces.

When the fix is a **choke point** — one guard every path crosses — it is
proved on the **complete enumeration** of what it claims to cover, each case
listed and executed, never on the reported site alone. A guard proved on one
case and covering the rest "by construction" has been bypassed on later rounds
by other cases from that same enumeration.

**(b) Does the mutation go red?** Put the defect back, require red **on that
test's own assertion**, restore. A test nobody has seen fail proves nothing.
And **mutate in the production path, never in the double**: if the oracle does
not honour the channel under test (a filesystem store that ignores the context
cannot observe a deadline), the bound is decorative even though the test is
red.

## 5. Re-attack the fix's diff before the gate

One agent, bounded to the diff of the fix itself. **The gate closes a loop; it
is not the reviewer of each individual fix** — five consecutive gate verdicts
at ≥ 1 medium is what skipping this looks like.

## 6. Feed the next round

Move handled findings into "already fixed" and argued refusals into "assumed".
That list is the input to the next round's prompt — keeping it current **is**
the work. A refusal parked in "assumed" that rests on a technical fact (out of
scope, load-bearing, impossible in production) carries its **executed proof**;
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

The three exits:

- **Requalify the scope** — a file that contains its own guards attacks itself.
- **Change the approach** rather than add an Nth patch.
- **Switch to a test of the GUARANTEE** — run the whole product under the
  hostile conditions the guard claims to neutralise and require the verdict —
  instead of enumerating the spellings the guard must recognise. A guard that
  ENUMERATES forbidden spellings never converges: the adversary is arbitrary
  text, so each round widens the pattern and the next finds one more way to
  write it.

Two demands that come with that switch: **check the bench can BITE** (a bench
that cannot go red is not a bench), and **stack the layers in order** — the
refusal carries the diagnosis, a check outlives the refusal, a floor outlives
the check; verify each by deleting the one above it.

## 8. Exiting the loop

**The gate's verdict closes the loop, not the round that declares it done.**
A sterile local round is the signal that it is time to push, never a stop
criterion: rounds a "sterile" criterion called finished have been followed by
real findings from the reviewer — the criterion counted rounds, the gate
counts defects.

So: push, then wait for `revi/review`. Its verdict is logged like a round and
its findings follow the same rules (class, mutation, verified fix). **Its
`questions`** — the falsifiability channel, non-blocking — **are
documentation findings**: each gets either a doc fix or an argued refusal in
the recap, never silence.

Where no reviewer is available, a local round of the full PR stands in for the
gate, and the recap says so.

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
