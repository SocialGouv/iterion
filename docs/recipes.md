[← Documentation index](README.md) · [← Iterion](../README.md)

# Recipes

Recipes let you run the same workflow with different configurations without editing the `.iter` file. They're useful for benchmarking models, comparing prompts, or creating reusable presets:

```json
{
  "name": "fast_review",
  "workflow_ref": {
    "name": "pr_refine_single_model",
    "path": "examples/pr_refine_single_model.iter"
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
iterion run workflow.iter --recipe fast_review.json
```

Recipes can override variables, prompts, budgets, and define success criteria for automated evaluation.
