# Backends and execution — the domain

Who runs the prompt, with which tools, and what the node keeps of its
native harness. Leaves, one line each:

| Leaf | Read it when |
|---|---|
| [backend-selection.md](backend-selection.md) | Wrong backend or wiring a new one. |
| [Capability registry](../../backend-capability-registry.md) | Backend facts, unchanged seed, derived enumerations and mutation witnesses. |
| [gateway.md](gateway.md) | Serving `openai_compatible/…`: its catalog, the price feed's platform/org/team seats and declarations, accepted efforts (run table, then env fallback), typed refusals. |
| [key-pools.md](key-pools.md) | Named credential pools attached per scope, the fallback strategy, an entry that never serves, debugging the margins (ADR-164). |
| [fallbacks-and-parity.md](fallbacks-and-parity.md) | `fallbacks:`, C173/C176, a "dumber" node, a degrading forfait. |
| [facade-substitution.md](facade-substitution.md) | GLM/Kimi served under a claude/gpt label, or a base URL the substitution verdict ignores. |
| [metering.md](metering.md) | Trusted delegate counters, reserved output metadata, interaction spend and recovery retry accounting. |
| [plugins-compression.md](plugins-compression.md) | Output compression (rtk), secret substitution, or a plugin changing what a node sees. |
| [codemode.md](codemode.md) | Script dispatch, context ownership and kernel confinement (ADR-165). |
| [codemode-discovery.md](codemode-discovery.md) | How a model learns a program's callable tools: the byte-stable index in the description, `call.describe`/`call.search`, the catalogue bounds (CM-3). |
| [Batches](codemode-concurrency.md) / [run state](codemode-state.md) | Parallel calls and the durable JSON bank. |
| [codemode-confinement.md](codemode-confinement.md) | The script host's ten bounds, the kernel mechanism behind each, the witness that was seen to fail without it, and the three statements measurement refuted. |
| [clawfold.md](clawfold.md) | Folding claw-code-go into `pkg/claw`: the computed membership, the per-file blob and inverse proofs, the slices and their floor, the switch and its residuals. |
| [sandbox.md](sandbox.md) | `SANDBOX_*` and `ATTACHMENT*` failures, host binds dropped or carried, changing what a node may execute. |
| [exec-environment.md](exec-environment.md) | Kubernetes per-call environment transport, native image pair, operator controls and qualification. |
| [prompt-engineering-dials.md](prompt-engineering-dials.md) | Authoring `cursor:` / `supervisor:` / ultracode. |
