# Backends and execution — the domain

Who runs the prompt, with which tools, and what the node keeps of its
native harness. Leaves, one line each:

| Leaf | Read it when |
|---|---|
| [backend-selection.md](backend-selection.md) | Wrong backend or wiring a new one. |
| [Capability registry](../../backend-capability-registry.md) | Backend facts, unchanged seed, derived enumerations and mutation witnesses. |
| [gateway.md](gateway.md) | Serving `openai_compatible/…` from the deployment env, its catalog, separate tolerant effort mapping, typed refusals, where provider/model config lives. |
| [key-pools.md](key-pools.md) | Named credential pools attached per scope, the fallback strategy, an entry that never serves, debugging the margins (ADR-164). |
| [fallbacks-and-parity.md](fallbacks-and-parity.md) | `fallbacks:`, C173/C176, a "dumber" node, a degrading forfait. |
| [facade-substitution.md](facade-substitution.md) | GLM/Kimi served under a claude/gpt label, or a base URL the substitution verdict ignores. |
| [metering.md](metering.md) | Trusted delegate counters, reserved output metadata, interaction spend and recovery retry accounting. |
| [plugins-compression.md](plugins-compression.md) | Output compression (rtk), secret substitution, or a plugin changing what a node sees. |
| [codemode.md](codemode.md) | The model's code calling the node's own tools: the dispatcher door and its context contract, why confinement is kernel-side, and what is NOT built yet (ADR-165). |
| [sandbox.md](sandbox.md) | `SANDBOX_*` and `ATTACHMENT*` failures, host binds dropped or carried, changing what a node may execute. |
| [prompt-engineering-dials.md](prompt-engineering-dials.md) | Authoring `cursor:` / `supervisor:` / ultracode. |
