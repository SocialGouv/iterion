---
name: architectural-axes
description: >
  A vocabulary of evolution axes Deep Blue draws from when synthesising a
  vision — reliability, performance, DX, modularity, and more. Free-form
  by design (not a closed enum); invent a new axis when the evidence
  needs one. Stack-agnostic.
disable-model-invocation: true
---

# Architectural axes — a vocabulary, not a checklist

An **axis** is one direction a project can evolve along. A vision picks
**3-6** axes the evidence supports — never all of them — and for each
states where the project is, where it should go, and why. This skill is
the vocabulary you draw from. It is **not** a closed list: when the repo
needs an axis that isn't here, name a new one.

For each axis below: what it means, the kind of evidence that justifies
it, and the guardrail that keeps it honest.

| Axis | What "evolving" it means | Evidence to look for | Guardrail |
|---|---|---|---|
| **Reliability** | Fewer failure modes; graceful degradation; recovery. | Crash/retry/timeout handling, error taxonomies, incident history. | Don't gold-plate reliability a young product doesn't need yet. |
| **Performance / scale** | Higher throughput or lower latency at the next order of magnitude. | Hot paths, profiling notes, N+1s, sync work that could be async. | Don't optimise before there's a measured bottleneck. |
| **Developer experience (DX)** | Faster, safer change for contributors. | Build/test times, setup friction, repetitive boilerplate, flaky tests. | Don't add tooling that itself needs maintaining for little gain. |
| **Observability** | Knowing what the system is doing in production. | Logging/metrics/tracing gaps, debugging pain in history. | Don't instrument everything; instrument what you'll actually read. |
| **Security posture** | Smaller attack surface, stronger boundaries. | Trust boundaries, input handling, secret management, audit logs. | Don't bolt on security theatre; target real boundaries. |
| **Modularity / decoupling** | Cleaner seams; smaller blast radius. | God-objects, circular deps, modules that change together. | Don't extract abstractions before the seam is proven (premature modularity). |
| **Extensibility** | A surface others can build on (plugins, adapters). | Repeated "add another X" patterns, hard-coded variants. | Don't build a plugin system for two cases. |
| **Operability** | Easier to deploy, configure, and run. | Deploy steps, config sprawl, manual runbooks. | Don't automate a process that runs twice a year. |
| **Data-model evolution** | Schema/migration strategy for the next stage. | Migration pain, denormalisation debt, versioning gaps. | Don't redesign the schema without a migration path. |
| **Documentation depth** | Closing the gap between code and its explanation. | Stale/missing docs, tribal knowledge, onboarding cost. | Docs follow stable design — don't document a moving target. |
| **Test strategy** | Coverage where it pays; the right test shapes. | Untested critical paths, slow/flaky suites, missing integration tests. | Coverage is a means, not a target — don't chase a percentage. |
| **Cost surface** | Lower run cost (compute, API, storage). | Expensive calls, idle resources, redundant work. | Don't micro-optimise cost at the expense of clarity. |

Other axes you may need (name them when the evidence calls for it):
multi-tenancy, internationalisation, on-call ergonomics, failure-mode
coverage, API ergonomics, supply-chain hygiene, migration/sunset paths.

## How to use this

1. From the survey evidence, pick the **3-6 axes the project's reality
   actually supports**. An axis with no concrete evidence is a guess —
   drop it or demote it to a guardrail.
2. For each, write `current_state → target_state` and cite real
   `evidence_paths` (files/dirs that justify it).
3. Make the axes **cohere**: a good vision is a few axes pulling the same
   direction, not a grab-bag. If two axes conflict (e.g. extensibility vs
   simplicity), say which wins and why.
4. The axes describe **direction**, not implementation. "Decouple the
   dispatcher from the runner" is an axis; "introduce interface X in file
   Y" is a tactic Nexie + feature-dev decide later.

## The guardrails matter as much as the axes

Every axis has an over-reach failure mode (see the table). A vision that
lists only ambitions and no guardrails is a wish-list, not a plan. State
what you will NOT pursue — that's how a vision survives contact with a
real, resource-constrained team.
