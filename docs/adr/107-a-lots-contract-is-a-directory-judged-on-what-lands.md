# ADR-107 — A lot's contract is a directory, judged on what lands

- **Status**: Accepted
- **Date**: 2026-09-30
- **Applies to**: `bots/modernize` (Morphy) — `plan_read`, `lot_verify`, `lot_gate`, `mark_done`; the `plan-contract` skill it shares with `bots/assessment`; every git command iterion runs (`pkg/git.NoRunHooks`), the landing of a run above all (`pkg/runtime`: `finalizeWorktree`, `PerformDeferredMerge`, `FinalizeConflictMerge`)
- **Neighbours**: [ADR-104](104-product-docs-route-table-resolved-once-at-the-run-base.md) (a verdict read from a commit, not from what the judged agent wrote)

## Context

The modernize bot carries one lot of a programme to its exit gate, and writes
`done` itself once the gate, the oracle and the references hold. The party the
contract binds must not be able to rewrite it. The bot refused a lot that
rewrote `plan.yaml`, and only for the keys it listed.

The contract a lot is judged against is wider than the plan:

- the programme's outcomes — each `check` is the command the programme verdict
  runs;
- the owner's brief — goals, decisions, permitted and forbidden changes;
- the arbitration doctrine a separate judge applies to blocked divergences;
- the defects register.

A lot that rewrote an outcome's check to `true` and renamed a brief goal
converged green.

Two more gaps sat in **when** and **on what** the contract was judged:

- **When.** It was judged once, before the exit_gate commands, and those
  commands run the lot's own code.
- **On what.** It was judged on the working tree, but what lands is committed.
  `mark_done` commits HEAD plus its status flip. A dirty tree is banked by the
  engine as a wip commit. The merge endpoint accepts a wip-banked run like any
  other: its merge status is `skipped`, a claimable state, and the run record
  does not say the head was banked.

Measured before this decision, on synthetic programmes, through the engine's
own finalize and deferred merge:

| lot | outcome |
|---|---|
| a gate command commits a contract rewrite | converged; `mark_done` committed `done` over it |
| a gate command leaves the rewrite uncommitted | converged, marked; the wip bank carried it, and a merge landed it on the target branch |
| a rewrite committed, then the file restored in the working tree | converged; `mark_done` committed `done` over the rewrite. The wip bank of the restore is what a squash landed |
| the same, with the restore marked `assume-unchanged` | converged; no wip bank, and the rewrite was auto-merged under `done` |

A review of a first implementation, which judged the working tree and HEAD,
measured five more ways a rewrite reached the landing under `done`:

- a rewrite staged in the **index** under `skip-worktree` or
  `assume-unchanged`, with the file on disk and HEAD both clean: `git add -A`
  leaves the entry alone, and the engine's own bank committed it;
- a **clean filter** keyed on `GIT_NO_REPLACE_OBJECTS`: the judge, which
  exports it, read the owner's bytes; the bank, which does not, stored the
  rewrite;
- a **pre-commit hook** that rewrote a contract file inside the bank's own
  commit;
- a worker commit storing the plan as a **symlink** (mode 120000) whose target
  is the plan's flipped text, taken for `mark_done`'s own earlier commit;
- an owner file beside the plan under **another name** than the five, judged by
  nothing.

The first rule for the lot's own records looked at the running lot's id
alone: a name starting with it and a separator was the lot's, and so was any
path the base did not hold. Measured on it:

- lot `L1` rewrote the record of an earlier lot `L1-b`, which its name
  matched (`L1-b-report.md` starts with `L1-`);
- lot `L1` created `sweeps/L22.md`, the record of a later lot. That lot's gate
  checks the record exists (`test -s .modernize/sweeps/L22.md`), so it held
  before `L22` had run;
- lot `L1` did the same with no file named for `L22`: `sweeps` linked to a
  directory of its own holding `L22.md`.

A third review measured three more; a fourth, five:

- the gestures that LAND a run — the squash commit and the fast-forward, at
  finalize or through `POST /merge`, and the commit of a resolved conflict —
  ran the repository's hooks. A `pre-commit` hook planted through the run's
  worktree rewrote the plan inside the landing commit; `merge_status` read
  `merged`, and the rewrite reached the operator's branch under `done`;
- a lot was marked `done` while the sweep record its own gate asserts
  (`test -s .modernize/sweeps/L1.md`) was in no commit: left uncommitted,
  ignored by a root `.gitignore`, or removed by a commit and put back on disk.
  The gate's commands read the working tree;
- the verdict grew with the directory: 2,000 captures gave a 279 kB
  `contract_tree`, carried on every pass.

The fourth review measured:

- the predicate demanded its record at the literal path in HEAD, so a base
  that ships `sweeps` as a link to the owner's records directory — frozen,
  as the limit above says — made every lot behind it unable to converge;
- the predicate held on the working tree, the index and HEAD, and the record
  still did not land: committed, then dropped from the index behind a root
  `.gitignore`, it was absent from the tree the engine's wip bank commits;
- the hookless bank push pushed Git LFS pointers and never their objects:
  git-lfs uploads them from the pre-push hook (measured with git-lfs 3.4.1;
  exit 0, and a fresh clone of the banked branch fails its checkout);
- a conflicted landing committed whatever the resolver staged — the
  contract's own plan included — with no re-judgement;
- the checkpoint and the workspace seeding still ran git as shell strings,
  with the run's hooks on.

A fifth review measured:

- the conflict re-judge compared the staged resolution with the pre-flip
  head: the run's own `done` plan was refused on the run's own landing;
- the bank's explicit LFS push carried a `<sha>:<refspec>` argument, which
  git-lfs cannot resolve: exit 0, no object uploaded;
- the banked-tree simulation spelled a tree-noise exclusion where git
  refuses it: an ignored `.claude` present untracked made every lot end in
  `CONTRACT_UNREADABLE`;
- the LFS detection listed paths quoted, and missed a non-ASCII name;
- a record HEAD carried behind its committed link and the landing carried
  at the literal path was refused though both trees held it.

## Options

- **(a) A list of forbidden edits per file.** A list protects what it names,
  and leaves the rest open. That is how `brief_targets` and the plan's
  top-level keys stayed writable.
- **(b) The owner files byte for byte, nothing else.** The register must take
  writes: a lot records the defects it finds and the fix of the ones it was
  given. A byte comparison refuses a reformatting and every legitimate record.
- **(c) One comparison, at `mark_done` only.** A refusal there ends the run
  instead of going back to the worker. And a rewrite is seen only after the
  full gate has run, which can take hours.
- **(D) One comparison, deny by default.** It covers the whole directory, runs
  before and after the gate commands, and judges both trees that land.
  `mark_done` commits only what that comparison judged.

## Decision

(D).

- **The contract is the plan's directory.** One table in `lot_verify` names
  five files, how each parses and what a lot may write in it: `plan.yaml`,
  `outcomes.json`, `brief.yaml`, `ARBITRAGE.md`, `defects-ledger.json`. Every
  other path the base holds in that directory is judged too, byte for byte.
- **Deny by default.** The table's files are compared with the run's base as
  parsed documents:
  - the owner files take no write;
  - the plan takes the running lot's own status, and new lots, which are
    proposals;
  - the register takes new entries, and the entries the lot names in
    `remediates:`. That list is read from the plan at the **base**, so a lot
    cannot widen its own licence.

  A removal, a duplicated id, a changed header, a symlink, a created or deleted
  owner file, or a duplicate key is a rewrite. A reformatting that parses to
  the same document is not. A table file that is not a regular file at the
  base, and a plan that is not a regular file at all, is a contract that
  cannot be read: what a link points at is not judged.
- **What a lot may write beside the plan.** Only its own records. A path
  beside the plan is **named for** a lot when one of its names carries that
  lot's id: the id itself, or the id followed by `-`, `.` or `_`.
  - The lots are the plan's at the base, plus the lots the landing adds
    (proposals). Reading the landing's lots cannot widen the running lot's
    licence: an id only takes names away from it.
  - The outermost name carrying an id decides: all of `<lot>-captures/` is
    that lot's, and so is `sweeps/<lot>.md`.
  - The longest id wins when several match: `L1-b-report.md` is `L1-b`'s,
    never `L1`'s, and `L10-report.md` is never `L1`'s.

  So the running lot may write:
  - any path named for it: `<lot>-report.md`, `<lot>-probe.py`,
    `sweeps/<lot>.md`, all of `<lot>-captures/`. Created, updated or removed;
  - any path the base does not hold that is named for no other lot: a record
    the lot creates.

  A path the lot creates that is named for **another** lot is refused: that
  lot's records are that lot's to write, and a sweep record written ahead of
  its lot answers that lot's gate before the lot has run. So is a **link** the
  lot creates under a name of no lot — symbolic, or to another repository: it
  stands for every path beneath it, another lot's record included (`sweeps`
  linked to a directory holding `L22.md`). Under its own id, a link is the
  lot's: every path beneath it is named for the lot. Everything else the base
  holds there — the owner's files, and the other lots' records — is
  byte-frozen. A file the owner wants protected is protected from the first
  run whose base carries it.
- **Judged on what lands.** Three trees are judged:
  - the working tree, as git would store it: `hash-object --path`, with the
    path's line-ending and encoding conversions applied;
  - the index, which a commit stores for an entry `git add` leaves alone
    (skip-worktree, assume-unchanged, a stat it trusts);
  - HEAD's committed tree.

  Two things a commit can store without either tree showing it are refused
  outright on a contract path — a table file, or a file the base holds that is
  not the running lot's — because they cannot be judged:
  - an index flag (skip-worktree, assume-unchanged);
  - a `filter` attribute. A clean filter is code of the judged party's
    choosing, run each time the path is stored, and it can answer the judge
    and the landing differently: the judge exports `GIT_NO_REPLACE_OBJECTS`,
    the engine's bank does not.

  A conversion git cannot apply to a contract path — a `working-tree-encoding`
  the file does not satisfy, a clean filter that fails — is refused by name,
  with the attribute: the file reads fine as raw bytes, and what git would
  store from it cannot be judged. Hashing writes the object, as a commit
  does: without `-w`, git prints the error, exits 0 and hashes the raw bytes.

  History is read as committed (`GIT_NO_REPLACE_OBJECTS`).
- **Judged twice:**
  - before any gate command, as a cheap refusal;
  - after the last one, which covers the gate commands, the oracle and the
    net's harness.

  A refusal after the gates goes back to the worker through the repair loop.
  `lot_gate`'s `converged` and `stop` both carry the contract as a term, so a
  refused verdict neither lands nor ends the run.
- **The gate's record predicate is judged on what lands.** A gate command that
  IS the predicate over a record in the contract's directory — `test -s
  <path>` or `[ -s <path> ]`, the whole command — holds on the working tree
  the commands read. After the last command, `lot_verify` requires the record
  as a non-empty file in the tree that LANDS:
  - at the path the commit really carries it — the committed links of the
    path are followed, so a base that ships `sweeps` as a link, and the lot's
    own links, are recorded behind, as the base dictates;
  - in HEAD, the commit `done` is written on;
  - in the tree the engine's bank would commit — the index as its staging
    rebuilds it from the working tree, the tree noise excluded. A record the
    index has dropped behind a root `.gitignore` is in no landing.

  Otherwise the gate fails by name, and says when git ignores the record. A
  record an earlier attempt committed and this one refreshed on disk lands.
  A record committed as a link resolves to the file the commit carries; a
  directory where the record's name goes is not a file.
- **`mark_done` commits only what was judged.** For the table's files and
  every path the landing carries in the directory (HEAD's tree, the index,
  the files `git add` takes), the verdict records what a commit of the working
  tree would take: the working tree as git would store it, the index entries
  and flags, the `filter` attribute. It carries the HEAD it judged, the five
  table files by name, and the rest of the directory as a count and a SHA-256
  of those states (canonical JSON). The verdict therefore stays under a
  kilobyte, whatever the lot's captures; it rides the checkpoint and the
  edges on every pass.
  - `mark_done` recomputes the same. It names a table file that moved, and
    otherwise says the rest of the directory moved: the same count with
    another state, or a path that appeared or went.
  - It applies no rule of its own: it only compares the landing with the
    verdict.
  Its own work is recognised exactly, so a re-execution after a crash stays
  idempotent:
  - its line in the plan: the plan the verdict judged in the working tree
    (proposals left uncommitted included), with the one line flipped;
  - its index entry for that plan;
  - its own commit on the judged HEAD: one parent, the judged HEAD, and one
    change, the plan at its base mode carrying the blob it commits. A commit
    storing the same bytes at another mode (a symlink whose target is the
    plan's text) is not its own.
- **No git iterion runs executes a repository hook.** A run writes its
  repository's hooks directory and config: a worktree shares them with the
  operator's checkout and every other run there, and a cloud run's clone is
  the run's own. A hook there, or the program `core.fsmonitor` names, is the
  run's code executed inside iterion's own gestures:
  - the landing of a run — squash, fast-forward, resolved conflict;
  - the wip bank and the salvage commit;
  - the bank's push;
  - a fork's checkout;
  - a delegation's snapshot.

  Every git command iterion builds therefore goes through
  `pkg/git.NoRunHooks`, which points `core.hooksPath` at nothing and turns
  `core.fsmonitor` off: `--no-verify` alone leaves `prepare-commit-msg`,
  `post-commit`, `post-merge` and `reference-transaction` running. A test
  sweeps the tree for every git subprocess and fails on one that does not.
  The studio's authoring is the one named exception: it commits for the user,
  in the user's own checkout. The gestures that run git as shell strings —
  the checkpoint's add, push and ls-remote inside the pod, the dispatcher's
  workspace seeding — carry the same options in their command lines, and the
  bot's own writes (`mark_done`'s ref update and index entry) run hookless
  too: the gate's word is not the run's to refuse, and the checkpoint is the
  net that preserves the run's work against the run.
- **A conflicted landing is re-judged.** The commit that lands a resolved
  conflict carries whatever the resolver staged — the contract's own plan
  included. Before it commits, the staged resolution is compared with the
  stored verdict: every contract file that verdict names must carry the bytes
  it judged. A resolution that rewrites the contract is refused by name —
  take the run's version, or land by hand. The owner changes the contract
  between runs, by hand, never inside a run's landing. Only the table's
  files are re-judged: the rest of the directory has no per-path record to
  compare with.
- **The bank pushes LFS objects first.** The bank's push runs no hook, and
  git-lfs uploads its objects from the pre-push hook: hookless, the push
  carries the pointers and never the objects. When the clone has an LFS
  filter configured and the tree tracks paths through it, the bank pushes
  the objects explicitly first (`git lfs push`, the bank's refspec only);
  when that upload fails, the bank is refused by name, never a silent
  pointer push.
- **One implementation.** Tool scripts share no code in the DSL: includes are
  for prompts only. So the comparison lives once, in `lot_verify`, and
  `mark_done` checks identity with the verdict rather than re-judging the
  contract.

## Consequences

- A contract change the programme needs is proposed by a lot, never made: an
  added lot, a paragraph in its report, or the owner's commit before the next
  run.
- A lot that records a fix in the register must be named for that entry in
  `remediates:` by the plan's author. A remediation lot drafted before this
  decision needs the field before it runs; without it, its record is refused
  by name.
- What a lot writes on an entry it remediates is not interpreted, because the
  register's vocabulary belongs to the programme. The lot's exit gate is where
  the required disposition is checked. A new entry may carry any disposition.
- Accepted limits, measured:
  - **What happens after `mark_done` is not judged by any node.** `mark_done`
    is the last node; the run's landing is made after it, by the engine.
    - On a **cloud run** the store has no working tree, so `finalizeWorktree`
      never runs: the runner banks and pushes **HEAD itself**. Its bank checks
      that the export is intact, not where HEAD stands relative to the commit
      `mark_done` reported. A process the lot left running can wait for the
      gate's commit subject and commit on top of it: the banked head carries
      the rewrite, above `done`, and no verdict of the run says so.
    - On a **local run** the engine banks the working tree as a wip commit on
      top of HEAD. Its merge status is `skipped`, which the merge endpoint
      claims like any other, and the run record does not say the head was
      banked. A write after `mark_done` rides that bank.

    No bot can close this. It needs the engine to refuse or flag a head that
    moved past the commit the terminal node reported. Until it does, a pilot
    merges a run only after re-judging the contract on the banked head.
  - The scripts an outcome's `check` runs (`ci/*`) are product files that lots
    legitimately write. They are not part of the contract.
  - A plan at the repository's root has no directory of its own: only the
    table's five files are the contract then.
  - The contract's files are found beside the plan. A brief kept elsewhere is
    not judged.
  - A supervising bot's own state kept under the plan's directory (a campaign
    journal) is frozen like any file the base holds there. A path it has not
    created yet is one a lot may create.
  - Names are matched against the plan's lot ids. A record named for an id no
    lot carries is no lot's: a lot may create it, and it is frozen once it has
    landed.
  - An untracked file git ignores is not part of the landing. Neither bank
    takes it: the local bank adds what `git add` takes, and a cloud run banks
    HEAD.
  - A link the base holds beside the plan is frozen as a link. What it points
    at is not judged: records kept through it live outside the contract.
  - Only hooks and `core.fsmonitor` are switched off. Other programs a
    repository's or the operator's git config can name still run in iterion's
    git: a merge driver, a smudge filter on checkout, `core.sshCommand`, and
    — on a host run, where iterion commits in the operator's own checkout —
    the program `commit.gpgsign`/`gpg.program` names to sign. A run that
    writes the shared repository's config can name any of these; the
    operator's own signing setup runs on every landing whether the run wrote
    anything or not. Measured: a run set a merge driver in the shared
    repository and declared it for `*.yaml` in `.git/info/attributes`. When
    the operator's branch had moved on the plan, the squash that landed the
    run called that driver, and the driver wrote the landed plan. Closing
    that class means landing from a checkout whose config the run never
    wrote.
  - The studio's authoring commits run the user's hooks. A run in the same
    repository can plant one there.
  - No `commit-msg` hook runs on a landing, so no Change-Id is added: a
    Gerrit-booking target cannot receive iterion's landings. There is no
    hooks path a run cannot write, so running the hook honestly is not
    possible; a Gerrit target is landed by hand.
  - A conflicted landing re-judges the table's files only. The rest of the
    contract's directory has no per-path record in the verdict; a resolution
    that rewrites another lot's record there is the owner's act, as between
    runs.
  - A name carries a lot's id as bytes do: case matters. `l1-report.md` is
    no record of lot `L1`'s, and `L1` owns nothing that `l1` does.
