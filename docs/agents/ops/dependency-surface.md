# Dependency surface — measure it before arguing about it

**Read it when** a proposal touches go.mod: replacing, inlining, forking or
cutting a dependency — or judging "how much third-party code do we actually
ship".

## The metric

`task deps:closure`
([scripts/deps-closure.sh](../../../scripts/deps-closure.sh)) resolves the
server binary's import closure (`go list -deps ./cmd/iterion`), keeps the
packages living under `vendor/` and `third_party/`, and prints their non-test
source LOC with the top contributors. It is source LOC, not binary size:
Go's dead-code elimination works at function granularity, so the number is an
upper bound on what ships — and the right unit for "generality we carry,
audit and bump".

**Baseline (2026-10-27, main `0b1de4062`): 7,340,738 LOC across 667
packages.** Composition of the top:

| block | linked LOC | why it is there |
|---|---|---|
| modernc sqlite + libc | ~5.6M | codex SDK fork's session persistence |
| tiktoken-go/tokenizer | ~0.4M | `pkg/backend/thinktokens` token counting |
| aws-sdk + smithy | ~0.21M | S3 client |
| otel + grpc + protobuf | ~0.19M | tracing |
| everything else | ~0.9M | the cores actually exercised |

## The rule it serves

- [ADR-142](../../adr/142-dependency-internalization-doctrine.md) — what may
  be internalized, cut or fork-patched, and what stays untouchable.
- [ADR-139](../../adr/139-language-stays-go-sidecars-for-the-rest.md) — the
  language itself is not on the table; other languages arrive as sidecars
  behind seams.

Cite before/after `deps:closure` numbers in any PR that changes go.mod.
