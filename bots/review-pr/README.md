# review_pr — Revi

Read-only cross-family **code reviewer**. Revi reviews the changes on the
current branch and *publishes* the findings — it never edits, fixes, or
commits code. (Fixing is the improve-loops' job: `branch_improve_loop` =
Billy, `whole_improve_loop` = Willy.)

Two independent reviewers from different model families (Claude + GPT)
audit the diff in parallel; a single `emit` step merges and de-duplicates
their findings, raises the confidence of anything **both** families
flagged ("cross-confirmed"), then writes one issue per finding to the
iterion native kanban board (labelled `severity:*`, `type:*`,
`source:revi`) plus a markdown report.

```
fan (fan_out_all)
  ├─ reviewer_claude   claude_code, read-only
  └─ reviewer_gpt      claw + openai/gpt-5.5, read-only tools
reviewer_* -> emit     await: wait_all  (merge + dedupe → board + report)
emit -> pr_gate        deterministic: was a pr_url given?
  ├─ no  -> done
  └─ yes -> publish_review   skill-guided forge PR review (inline
            -> publish_health  comments + ```suggestion) -> done
```

## Scope

Reviewers audit `git diff $(git merge-base {{base_ref}} HEAD)` — the
**working-tree** diff against the merge-base, so both committed and
uncommitted branch changes are reviewed. To review **only** the
uncommitted working tree, run with `--var base_ref=HEAD`.

## Inputs

All inputs are workflow `vars` (override with `--var name=value`):

| Var | Default | Description |
|---|---|---|
| `workspace_dir` | `${PROJECT_DIR}` | Repo to review (the run's workspace). |
| `base_ref` | `main` | Ref to diff against (`merge-base(base_ref, HEAD)` vs working tree). `HEAD` = uncommitted only. |
| `scope_notes` | `""` | Free-text steering passed to both reviewers. |
| `severity_threshold` | `low` | Drop findings below this (low < medium < high < critical). |
| `max_findings` | `40` | Cap on issues/rows (highest severity first); a capped run says so. |
| `post_to_board` | `true` | File findings on the native board; `false` = report only. |
| `report_path` | `.review-pr/findings.md` | Markdown report destination (gitignorable; not under `.iterion/`). |
| `pr_url` | `""` | When set, ALSO publish the review onto this PR (see below). Empty = board + report only. |

## Run

```bash
iterion run bots/review-pr/main.bot \
  --var workspace_dir=/path/to/repo \
  --var base_ref=main
```

Findings land on the native board under the label `source:revi`; the
markdown report is written to `report_path`. Surface a run's output with
`iterion report --run-id <id>`.

## Publish onto a forge PR (`--var pr_url=…`)

Give Revi a pull-request URL (merge request on GitLab) and it ALSO posts its findings onto
that PR as a real forge review — one inline comment per finding anchored
to `file:line`, with a one-click ` ```suggestion ` block when the
finding carries a concrete replacement, plus a summary comment. The
board + report still run; the PR review is additive.

```bash
# Check out the PR's branch locally, then:
iterion run bots/review-pr/main.bot \
  --var workspace_dir=/path/to/repo \
  --var base_ref=main \
  --var pr_url=https://github.com/owner/repo/pull/42
```

- **Forge-agnostic.** The publish step reads the `forge-pr-review.md`
  skill and dispatches by the URL host — GitHub (`gh`), GitLab (`glab`),
  or Forgejo/Gitea (REST API). No forge names are baked into the
  workflow. The matching CLI must be authenticated on the host (e.g.
  `gh auth login`).
- **Diff model (v1).** Revi reviews the LOCAL checkout (`base_ref..HEAD`)
  and publishes to `pr_url`; check out the PR branch and pass its base
  as `base_ref`. Auto-resolving base/head from the URL is a planned
  enhancement.
- **Anti-façade.** `publish_review` re-fetches the posted review to count
  the comments the forge actually stored, and a deterministic
  `publish_health` gate raises a loud banner if findings existed but zero
  comments landed (e.g. the CLI was not authenticated) — the board +
  report still succeed, so fix forge auth and re-run with the same
  `pr_url`.

## Read-only by construction

No node mutates source: `reviewer_claude` is `readonly: true` (Write/Edit
removed, Read/Grep/Bash kept for `git diff`), and `reviewer_gpt` is given
only read tools (`bash`, `read_file`, `glob`, `grep` — no
`write_file`/`file_edit`). The single downstream `emit` step writes only
the report file and creates board issues over MCP.

See [main.bot](main.bot) for the full DSL.
