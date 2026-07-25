# Recipes

A recipe pins one `.bot` to a named configuration — preset vars, prompt overrides, a budget, and a success metric — so you can benchmark models, A/B prompts, or ship a reusable preset without ever touching the workflow source:

```json
{
  "name": "fast_review",
  "workflow_ref": {
    "name": "branch_improve_loop",
    "path": "bots/branch-improve-loop/main.bot"
  },
  "preset_vars": {
    "review_rules": "Focus on security only"
  },
  "prompt_pack": {
    "review_system": "You are a security-focused reviewer."
  },
  "budget": {
    "max_duration": "10m",
    "max_cost_usd": 5.0
  },
  "evaluation_policy": {
    "primary_metric": "approved",
    "success_value": "true"
  }
}
```

```bash
iterion run workflow.bot --recipe fast_review.json
```

Recipes can override variables, prompts, budgets, and define success criteria for automated evaluation.
