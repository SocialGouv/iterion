# branch_improve_loop

Branch-scoped variant of `whole_improve_loop`. Runs the alternating
Claude/GPT review-fix loop against the diff between a feature branch
and its base, auto-commits each fix, and stops on cross-family
double-approval.

## Inputs

| Var | Required | Description |
|---|---|---|
| `repo_path` | yes | Absolute path to the target repo |
| `base_branch` | yes | Branch to diff against (e.g. `main`) |
| `feature_branch` | no | Branch under review; defaults to the current branch |

## Run

```bash
iterion run examples/branch_improve_loop/main.bot \
  --var repo_path=/path/to/repo \
  --var base_branch=main
```

See [main.bot](main.bot) for the full DSL.
