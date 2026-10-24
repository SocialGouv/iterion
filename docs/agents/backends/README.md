# Backends and execution — the domain

Who runs the prompt, with which tools, and what the node keeps of its
native harness. Leaves, one line each:

| Leaf | Read it when |
|---|---|
| [backend-selection.md](backend-selection.md) | Wrong backend or wiring a new one. |
| [gateway.md](gateway.md) | Serving `openai_compatible/…` from the deployment env, its catalog, its refusals. |
| [fallbacks-and-parity.md](fallbacks-and-parity.md) | `fallbacks:`, C173/C176, a "dumber" node, a degrading forfait. |
| [metering.md](metering.md) | Trusted delegate counters, reserved output metadata, interaction spend. |
| [plugins-compression.md](plugins-compression.md) | Output compression (rtk), secret substitution, or a plugin changing what a node sees. |
| [sandbox.md](sandbox.md) | `SANDBOX_*` failures, changing what a node may execute. |
| [prompt-engineering-dials.md](prompt-engineering-dials.md) | Authoring `cursor:` / `supervisor:` / ultracode. |
