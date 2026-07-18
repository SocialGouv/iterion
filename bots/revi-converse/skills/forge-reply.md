---
name: forge-reply
description: How to POST a reply note in an EXISTING pull-request (merge-request on GitLab) discussion thread on GitHub, GitLab, or Forgejo/Gitea. Read this before posting the answer note. Enforces the anti-trigger-loop rule (never begin the body with `/revi` or `/ask`).
---

# Posting an answer in an existing PR discussion thread

You are answering an operator's question on an open PR. The answer must
land as a REPLY in the SAME discussion thread the operator's note opened
(`{{vars.discussion_id}}`), not as a new top-level comment. You POST one
note — you NEVER edit, fix, or commit the workspace.

Reading the thread: the webhook handler injects the discussion transcript
as `{{vars.thread_context}}`, so you normally do NOT need to fetch it.
When that var is EMPTY (manual run, fetch failure) and the question
references prior discussion, READ the thread first with the same GET the
VERIFY step uses (GitLab: §3's re-fetch call; GitHub:
`GET .../pulls/:number/comments` filtered on the thread; Forgejo/Gitea:
`GET .../issues/:index/comments`), then answer.

## 0. Anti-trigger-loop rule (CRITICAL — every forge)

The forge webhook re-fires on a comment whose FIRST non-whitespace token
is `/revi` (or `/ask` once added). If you post a body that starts with
`/revi`, the forge fires the webhook back to Revi, which posts another
note, which re-fires, forever. SAFE phrasings:

- Quote it: `` Use `/revi` to retry. ``
- Re-anchor it: `Re-trigger with /revi.` (token not first).
- Mention it mid-sentence: `You can run /revi for a fresh pass.`

VERIFY YOURSELF before posting: the first non-whitespace character of the
body MUST NOT be `/`, OR if it is, the next token MUST NOT be `revi`,
`Revi`, `REVI`, `ask`, `Ask`, `ASK`. When in doubt, prepend a single
prose token (e.g. "Re:" or "Answer:") so the body opens with normal
text.

## 1. Detect the forge from the PR URL

Parse the URL host:

- `github.com` (or GitHub Enterprise host) → **GitHub**, REST API.
- host contains `gitlab` → **GitLab**, REST API.
- otherwise assume **Forgejo / Gitea** → REST API.

Derive `<host>`, `<group>/<project>` (or `<owner>/<repo>`) and the
PR iid/number directly from `{{vars.pr_url}}`. URL-encode the
project path when calling GitLab.

## 2. Read the forge_token (file secret)

The token is mounted at `/run/iterion/secrets/forge_token` (declared
via `secrets: { forge_token: { as: file, optional: true } }` in the bot's
DSL). Read it with `cat /run/iterion/secrets/forge_token` and pass it as
a header — NEVER read its value into a prompt or echo it.

If the file is missing on a manual local run, skip posting and set the
output `posted=false` with a precise `skipped_reason` (e.g.
`"forge_token missing; this run is unattended-only"`).

## 3. GitLab — REPLY in an existing discussion

Reply to an existing MR discussion thread via:

```
POST projects/:id/merge_requests/:iid/discussions/:discussion_id/notes
```

Reuse the curl + nested-JSON pattern (avoids glab's flat-key bug for
position objects; mirrors the post pattern in `forge-pr-review.md`):

```sh
TOKEN="$(cat /run/iterion/secrets/forge_token)"
HOST="<host-from-pr_url>"             # e.g. gitlab.com
PROJECT="<URL-encoded group%2Fproject>"
IID=<mr-iid>
DISCUSSION="{{vars.discussion_id}}"
BODY="$(cat /tmp/revi-answer.md)"      # the answer text — see §6

jq -nc --arg body "$BODY" '{body: $body}' \
  | curl -sS -H "PRIVATE-TOKEN: $TOKEN" -H 'Content-Type: application/json' \
      -X POST "https://${HOST}/api/v4/projects/${PROJECT}/merge_requests/${IID}/discussions/${DISCUSSION}/notes" \
      -d @-
```

If `jq` is missing (exit 127 on a minimal host), build the payload with
python3 instead — same POST, no other change:

```sh
python3 -c 'import json; print(json.dumps({"body": open("/tmp/revi-answer.md").read()}))' > /tmp/revi-payload.json
curl -sS -H "PRIVATE-TOKEN: $TOKEN" -H 'Content-Type: application/json' \
  -X POST "https://${HOST}/api/v4/projects/${PROJECT}/merge_requests/${IID}/discussions/${DISCUSSION}/notes" \
  -d @/tmp/revi-payload.json
```

VERIFY (mandatory): re-fetch the discussion and confirm a new note
landed by capturing its id + URL.

```sh
curl -sS -H "PRIVATE-TOKEN: $TOKEN" \
  "https://${HOST}/api/v4/projects/${PROJECT}/merge_requests/${IID}/discussions/${DISCUSSION}" \
  | jq '.notes[-1] | {id, body, created_at}'
```

Report the discussion URL (`{{vars.pr_url}}#note_<id>`) as `reply_url`.

## 4. GitHub — REPLY on an existing PR review-comment thread

GitHub threads PR conversations off a top-level review comment. The
reply endpoint is:

```
POST repos/:owner/:repo/pulls/:number/comments/:comment_id/replies
```

Where `comment_id` is the id of the FIRST comment in the thread —
`{{vars.discussion_id}}` carries that id when the operator replied to a
Revi review comment. When the operator instead opened a brand-new issue
comment (no review-comment thread to reply to), `discussion_id` will
NOT map to a review-comment id; in that case fall through to an
issue-comment reply that quotes the original note:

```sh
TOKEN="$(cat /run/iterion/secrets/forge_token)"
HOST="api.github.com"                  # or GHE host
OWNER="<owner-from-pr_url>"
REPO="<repo-from-pr_url>"
NUMBER=<pull-number>
PARENT="{{vars.discussion_id}}"        # GitHub review-comment id
BODY="$(cat /tmp/revi-answer.md)"

# Try in-thread reply first (review-comment thread).
HTTP=$(curl -sS -o /tmp/revi-reply.json -w '%{http_code}' \
  -H "Authorization: Bearer ${TOKEN}" -H "Accept: application/vnd.github+json" \
  -X POST "https://${HOST}/repos/${OWNER}/${REPO}/pulls/${NUMBER}/comments/${PARENT}/replies" \
  -d "$(jq -n --arg body "$BODY" '{body: $body}')")

if [ "$HTTP" != "201" ]; then
  # Fall back to an issue-comment that quotes the original note.
  curl -sS -H "Authorization: Bearer ${TOKEN}" -H "Accept: application/vnd.github+json" \
    -X POST "https://${HOST}/repos/${OWNER}/${REPO}/issues/${NUMBER}/comments" \
    -d "$(jq -n --arg body "$BODY" '{body: $body}')"
fi
```

VERIFY: capture the returned `html_url` as `reply_url`.

## 5. Forgejo / Gitea — REPLY on an existing PR

Forgejo/Gitea expose issue-style comments on PRs. There is no
"discussion thread" REST endpoint that mirrors GitLab's; the cleanest
universal answer is a new issue-comment that quotes the operator's
note for context, posted into the same PR conversation:

```sh
TOKEN="$(cat /run/iterion/secrets/forge_token)"
HOST="<host-from-pr_url>"
OWNER="<owner-from-pr_url>"
REPO="<repo-from-pr_url>"
INDEX=<pr-index>
BODY="$(cat /tmp/revi-answer.md)"

curl -sS -X POST \
  -H "Authorization: token ${TOKEN}" -H "Content-Type: application/json" \
  "https://${HOST}/api/v1/repos/${OWNER}/${REPO}/issues/${INDEX}/comments" \
  -d "$(jq -n --arg body "$BODY" '{body: $body}')"
```

VERIFY: re-fetch `GET .../issues/:index/comments` and grab the last
entry's `html_url` as `reply_url`.

## 6. The answer body — shape

The body is short, focused, GROUNDED. Operators are senior engineers
asking a sharp question; an essay is worse than a paragraph.

```
Re: {{vars.replier}} — "<short echo of the question>"

<3-8 sentences answering. Anchor every claim to file:line in the diff
or an explicit "based on the PR description" / "based on Revi's
earlier finding X" attribution. Quote ≤3 lines of code with a markdown
fence when it sharpens the answer.>

<If the answer requires a follow-up action, name it explicitly — e.g.
"Run `/revi` for a fresh review after the fix lands.">
```

Do NOT:
- begin the body with `/revi` (see §0 — re-anchor or quote it).
- promise to "look at it later" — answer with what you have NOW.
- re-emit the full Revi review (that is review-pr's job).
- hallucinate file paths or line numbers — every anchor must exist in
  the diff or in the surrounding source you actually read.

## 7. Output contract

The bot's structured output (`converse_output`):

- `posted`: true only when the forge accepted the reply (HTTP 2xx and
  the VERIFY step found the new note).
- `forge`: `"github" | "gitlab" | "forgejo" | "unknown"`.
- `reply_url`: link to the posted note (from the VERIFY step).
- `skipped_reason`: non-empty when nothing was posted (token missing,
  forge unrecognised, discussion thread closed, etc.).
- `summary`: one paragraph — the question, the gist of the answer, and
  the VERIFY call you ran. A success claim without a VERIFY call is a
  façade and must be reported as a partial success.

## 8. Forward direction (NOT this skill yet)

The elegant follow-up is a `forge.reply` DSL capability — sibling of
`board.create` — that opens an in-process `forge_reply(thread_id, body)`
tool and lets iterion own the posting (rather than `curl` in a skill).
See `docs/forge-conversations.md` §A4. Until then, this skill is the
contract.
