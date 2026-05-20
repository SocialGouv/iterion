# feature_dev

Autonomous end-to-end feature development. Plans, implements, simplifies,
then runs the alternating Claude/GPT review-fix loop until two
consecutive cross-family approvals.

## Inputs

| Var | Required | Description |
|---|---|---|
| `feature_prompt` | yes | High-level description of the feature to implement |
| `repo_path` | yes | Absolute path to the target repo (worktree-aware) |
| `max_review_iterations` | no | Cap on review-loop iterations (defaults set in the bot) |

## Pipeline

1. `plan` — Claude Code, read-only exploration → structured plan
2. `act` — Claude Code, session-inherit → implements the plan
3. `simplify` — Claude Code, native `/simplify` skill on the new code
4. `alt → reviewer_claude / reviewer_gpt → streak_check → fixers` —
   verbatim copy of `whole_improve_loop` after the dev phase, stops on
   cross-family double approval

## Run

```bash
iterion run examples/feature_dev/main.bot \
  --var feature_prompt='Add a /healthz endpoint that returns build info' \
  --var repo_path=/path/to/repo
```

See [main.bot](main.bot) for the full DSL.
