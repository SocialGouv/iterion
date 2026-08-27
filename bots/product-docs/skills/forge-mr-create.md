---
name: forge-mr-create
description: How to push the current run's branch and open ONE pull request (merge request on GitLab) on GitHub (gh), GitLab (glab) or Forgejo/Gitea (REST), authenticating from the mounted forge_token, then post the PR URL back to the source issue. Read this before the finalize_mr node pushes or opens anything.
---

<!-- DIVERGED copy — this file carries product-docs' own DRAFT delta on
     top of the skill the other bundles byte-share (iterion has no
     skill-sharing primitive; see CLAUDE.md "If a skill ends up
     duplicated across multiple bundles"). Do NOT blind-sync it from the
     shared copies (app-dev / branch-improve-loop / feature-dev /
     instrument / whole-improve-loop) — port shared fixes by hand and
     keep the delta. bots/docs-refresh/skills/forge-mr-create.md carries
     a different divergence (amend mode). -->

# Opening a pull request from a finished run

You are turning the commits this run produced into ONE pull request
(PR; merge request on GitLab), then optionally posting its URL back onto
the source issue. You push and open a PR — you do NOT edit, fix, or
re-commit the workspace.

## Fresh repository (created for this run — no remote branches yet)

When the run targeted a repository CREATED at launch, `origin` exists
but has no branches (`git ls-remote --heads origin` is empty). There is
no base to open a PR against — the FIRST push IS the publication:

1. `git push -u origin HEAD` — publishes the current branch; on an
   empty repo it becomes the default branch.
2. Skip the PR entirely (a PR needs base ≠ head). Report the repo's
   web URL as the deliverable instead of a PR URL.
3. Only from the SECOND run onward (remote default branch exists) does
   the normal branch → PR flow below apply.

## 0. Resolve the base, then check there are commits to ship

The CWD is the run's git worktree (or clone). First resolve the BASE the
PR will target. Use the `mr_base` input when it is non-empty; otherwise
resolve the repository's DEFAULT branch from the origin remote — never
hardcode `main`:

```
BASE="$mr_base"                                  # from input; may be empty
if [ -z "$BASE" ]; then
  # Default branch as the remote advertises it (origin/HEAD).
  BASE="$(git symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's@^refs/remotes/origin/@@')"
fi
if [ -z "$BASE" ]; then
  # origin/HEAD not set (fresh clone / no remote HEAD ref). Ask the remote.
  BASE="$(git remote show origin 2>/dev/null | sed -n 's/.*HEAD branch: //p')"
  # Cache it so the symbolic-ref lookup works next time.
  [ -n "$BASE" ] && git remote set-head origin "$BASE" >/dev/null 2>&1 || true
fi
BASE="${BASE:-main}"                             # last-resort floor only
```

Then confirm HEAD is ahead of that base before doing anything. Compare
against the REMOTE ref, never the local branch: in a cloud/runner clone
the campaign commits directly on the checked-out base branch, so the
local `$BASE` ref IS HEAD and `"$BASE"..HEAD` is vacuously 0 — that
exact mistake silently discarded a run's real commits.

```
git fetch origin "$BASE" --quiet 2>/dev/null || true
git rev-list --count "origin/$BASE"..HEAD        # > 0 means there is work to ship
```

If the count is 0 (or `origin/$BASE` is missing), STOP: return
`opened=false` with `skipped_reason="no commits ahead of base"`. Never
open an empty PR.

## 1. Detect the forge from the origin remote

```
git remote get-url origin
```

- host `github.com` (or GitHub Enterprise) → **GitHub**, use `gh`.
- host contains `gitlab` → **GitLab**, use `glab`.
- otherwise assume **Forgejo / Gitea** (self-hosted) → REST API via `curl`.

If there is no `origin` remote (a local-only run), return `opened=false`
with `skipped_reason="no origin remote to push to"`.

## 2. Authenticate from the mounted forge_token

This run may be unattended (a comment-triggered launch) with no
pre-authenticated forge CLI, and a reused runner pod can carry a PREVIOUS
run's login. The most robust path is **environment auth**: gh/glab read a
token straight from the env, which overrides any stale login and avoids the
`auth login` / `set-token` sub-commands whose flags drift between CLI
versions (glab's `--host` → `--hostname` rename has bitten real runs). If
the mounted secret file `/run/iterion/secrets/forge_token` EXISTS, export it
for the matching CLI FIRST; never read the token into a prompt or echo it:

- GitHub: `export GH_TOKEN="$(cat /run/iterion/secrets/forge_token)"`
- GitLab: `export GITLAB_TOKEN="$(cat /run/iterion/secrets/forge_token)"` plus
  `export GITLAB_HOST="<host>"` (host parsed from the origin URL); glab reads
  both — do NOT run `glab auth login` / `set-token`.
- Forgejo/Gitea: `export FORGEJO_TOKEN="$(cat /run/iterion/secrets/forge_token)"`

When the file is absent (manual/local runs), assume host auth and verify
the CLI is ready (`gh auth status` / `glab auth status`). If neither a
token file nor host auth is available, push/open nothing: return
`opened=false` with a precise `skipped_reason` (e.g.
`"gh not authenticated; run gh auth login"`). Do not pretend.

## 3. Choose the branch and push

Pick the branch name: use the `mr_branch` input if non-empty, else derive
a stable one — `iterion/product-docs/<run-id-or-short-sha>`. Push the run's
HEAD to it on origin:

```
BRANCH="${mr_branch:-iterion/product-docs/$(git rev-parse --short HEAD)}"
git push origin "HEAD:refs/heads/$BRANCH"
```

If the push is rejected (protected branch, no permission), return
`opened=false` with the precise `skipped_reason` from the push error.

## 4. Open the pull request

Title: a short semantic summary of the improvements (reuse the run's
commit subjects / scope_notes). Body: what changed and why, plus a line
noting it was generated by an iterion improvement run.

- **GitHub:**
  `gh pr create --base "$BASE" --head "$BRANCH" --title "<title>" --body "<body>"`
  (prints the PR URL on success). If a PR already exists for the branch,
  `gh pr view "$BRANCH" --json url -q .url` — treat as `opened=true`.
- **GitLab:**
  `glab mr create --source-branch "$BRANCH" --target-branch "$BASE" --title "<title>" --description "<body>" --yes`
  (prints the MR URL). Idempotent: if an MR already exists, fetch its URL
  with `glab mr view "$BRANCH" -F json` and reuse it.
- **Forgejo / Gitea (REST):** parse `<owner>/<repo>` from the origin URL,
  then `POST https://<host>/api/v1/repos/<owner>/<repo>/pulls` with body
  `{"head":"<BRANCH>","base":"<BASE>","title":"<title>","body":"<body>"}`
  and header `Authorization: token $FORGEJO_TOKEN`. The response JSON's
  `html_url` is the PR URL.

Capture the resulting URL into `url` and set `opened=true`, `branch=$BRANCH`.

## 4b. DRAFT — the product-docs delta

Functional documentation is validated by the people who own the product,
on the forge, before it is merged. So when the `draft` input is true
(the default), the pull request is opened as a **draft** and left that
way: marking it ready is the reviewer's act, never the bot's.

- **GitHub:** add `--draft` to `gh pr create`.
- **GitLab:** add `--draft` to `glab mr create` (older `glab` builds
  instead want the title prefixed with `Draft: `; if `--draft` is
  rejected, retry with the prefix).
- **Forgejo / Gitea (REST):** there is no draft field on the create
  endpoint — prefix the title with `WIP: `, which is Forgejo's own
  work-in-progress marker and blocks the merge button the same way.

An existing PR found on the branch is reused as-is: do **not** flip a PR
a human already marked ready back to draft.

If the draft could not be honoured (an old CLI, a provider with no draft
concept and no WIP convention), open the PR anyway and say so in
`skipped_reason` — `"opened, but not as a draft: <reason>"`. Report the
truth in the `draft` output field: it says whether the PR **is** a
draft, not whether one was asked for. Silently dropping the draft intent
is the one outcome that is not acceptable — a reviewer who expects a
draft may otherwise merge unvalidated prose.

## 5. Back-link the source issue (optional)

If the `source_issue_ref` input is non-empty, post the PR URL back to it
so the requester sees the result where they asked:

- ref looks like a **forge issue URL** (`https://…/issues/<n>` or
  `…/-/issues/<n>`): comment via the same CLI —
  `gh issue comment <n> --body "Opened: <url>"` /
  `glab issue note <n> --message "Opened: <url>"` / Forgejo
  `POST /repos/<owner>/<repo>/issues/<n>/comments`.
- ref looks like **`native:<id>`** (the local kanban board): call the
  board comment tool — `mcp__iterion_board__comment_issue` with
  `{ id: "<id>", body: "Opened PR: <url>" }` (the finalize_mr node holds
  the `board.comment` capability for exactly this).

Set `back_linked=true` only when the comment actually posted. A failed
back-link is not fatal — keep `opened=true` and note the failure in
`summary`.

## Honesty contract

Report exactly what happened: `opened`, `url`, `branch`, `draft`,
`back_linked`, and a precise `skipped_reason` whenever `opened=false` (or
whenever the draft could not be honoured). Never fabricate a URL, never
claim a PR you did not open, never claim a draft that is not one, never
echo the token.
