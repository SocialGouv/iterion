# Authoring bots — the four rules a catalog bot must clear

A bot shipped in `bots/` is a general-purpose tool. It must be **repo-agnostic**
(no iterion paths baked in), **stack-agnostic** (adding a language is dropping a
skill file), convergent (an asymptote, not an oscillation), and declare the
tools it needs. The mirror rule — **the engine must never know a specific
bot** — is here too, because the two are one boundary seen from both sides.

Read [../workflow_authoring_pitfalls.md](../../workflow_authoring_pitfalls.md)
before writing or amending any `.bot` that can commit code.


## The leaves

[Skills live with their bundle](skills.md) · [loops converge](convergence.md) ·
[the UNTRUSTED INPUT BOUNDARY](untrusted-input.md) ·
[repo- and stack-agnostic](universality.md) ·
[the ENGINE stays bot-agnostic](engine-bot-agnostic.md) ·
[a bot's toolchain is its `devbox.json`](toolchain-devbox.md).
