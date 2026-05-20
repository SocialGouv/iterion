[← Documentation index](README.md) · [← Iterion](../README.md)

# Examples

The [`examples/`](../examples/) directory ships a curated set of
proven, productized bots and one actively-developed workflow.

> **Extensions.** Iterion accepts both `.iter` and `.bot`. We use
> `.iter` for raw DSL or work-in-progress workflows and `.bot` for
> productized operational bots — workflows meant to run unmodified
> against real systems with human gates, mitigation steps and reports
> (see [`.iter` vs `.bot`](../README.md#iter-vs-bot)). The parser,
> runtime, and editor treat them identically.

## 🤖 Productized bots (folder-per-bot under [`examples/`](../examples/))

| File | Description |
|------|-------------|
| [`feature_dev/`](../examples/feature_dev/) | Self-driven feature development bot: plan → implement → review → refine loop with judge gates |
| [`whole_improve_loop/`](../examples/whole_improve_loop/) | Alternating Claude/GPT review-and-fix pattern with cross-family streak detection (whole-repo scope) |
| [`branch_improve_loop/`](../examples/branch_improve_loop/) | Branch-scope variant of the alternating improve loop with auto-commit between iterations |

## 🚧 In active development

| File | Description |
|------|-------------|
| [`secured-renovacy/`](../examples/secured-renovacy/) (`.botz` bundle) | Autonomous, security-aware dependency upgrades for any stack (yarn/npm/pnpm/pip/poetry/uv/cargo/go/bundler/composer/maven). Run via `iterion run examples/secured-renovacy/` or against the packed archive `iterion bundle pack examples/secured-renovacy && iterion run examples/secured-renovacy.botz`. |

## 📦 Archived

Older `.iter` workflows that were useful at one time but are no longer
maintained have moved to [`.archive/examples/`](../.archive/examples/).
They are not embedded in the binary and are kept only for historical
reference and as fixtures for the test suite.
