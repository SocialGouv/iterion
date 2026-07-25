---
name: threat-model
description: |
  Build (or refresh) the project threat model that Seki consumes
  before triage and ranking. Three modes: `interview` (walk an owner
  through the four-question framework), `bootstrap` (derive the
  model from the code + past vulns), and `bootstrap-then-interview`
  (chain the two). Output lands in `.sec-audit/context.md`.
attribution: |
  Adapted from Anthropic's `defending-code-reference-harness`
  (`/threat-model` reference implementation). Re-anchored on
  iterion's finding taxonomy + kanban-based findings, the C/C++/ASAN
  framing replaced by the Go/TS scanner surfaces Seki actually
  audits.
---

# threat-model — what could go wrong in this codebase

A threat model answers **"what could go wrong with this system, who
would do it, and what should we do about it?"** *independently* of
whether any specific bug has been found yet. It is the map; Seki's
scanners + revalidate are the metal detector. A good threat model
tells Seki where to look and tells `[[finding-taxonomy]]` which
findings to escalate.

**Litmus test.** If patching one line of code makes an entry
disappear, it was a vulnerability, not a threat. A threat ("attacker
achieves IDOR via missing tenant filter") still stands after every
known bug is fixed; a vulnerability ("`pkg/api/orders.go:212` doesn't
filter by `company_id`") does not. **Threats persist after patching;
vulnerabilities disappear.** This skill produces threats.
Vulnerabilities appear only as **evidence** that raises a threat's
likelihood.

**Why we bother.** A well-defined threat model raises the share of
*exploitable* findings Seki surfaces to ~90%. Without one, the
revalidate judge is operating blind on a generic untrusted-input
boundary and the kanban fills with theoretical CVEs.

## Output contract — `.sec-audit/context.md`

This skill writes **one file** per workspace:

```
<workspace_dir>/.sec-audit/context.md
```

It is markdown so humans can edit it; the section headings + table
columns are a contract Seki's triage and revalidate phases parse.
Do NOT emit JSON; that was the upstream harness pattern. Seki reads
the markdown directly.

Required sections, in order:

```markdown
# Threat Model: <system name>

## 1. System context
## 2. Assets
## 3. Entry points & trust boundaries
## 4. Threats
## 5. Deprioritized
## 6. Open questions
## 7. Provenance
## 8. Recommended mitigations
```

### Section shapes

- **1. System context** — 1–3 paragraphs of prose: what the system
  is, who uses it, where it runs. No table.
- **2. Assets** — table `| asset | description | sensitivity |`,
  `sensitivity ∈ {low, medium, high, critical}`.
- **3. Entry points & trust boundaries** — table
  `| entry_point | description | trust_boundary | reachable_assets |`.
- **4. Threats** — table
  `| id | threat | actor | surface | asset | impact | likelihood | status | controls | evidence |`.
  - `id`: `T1`, `T2`, … stable across edits.
  - `threat`: one sentence, active voice ("Cross-tenant data leak via
    missing `company_id` filter on `/api/orders`"), not the bug class.
  - `actor` ∈ `remote_unauth | remote_auth | adjacent_network |
    local_user | supply_chain | insider`.
  - `surface`: entry point(s) from section 3.
  - `asset`: asset(s) from section 2.
  - `impact` ∈ `low | medium | high | critical | existential`.
  - `likelihood` ∈ `very_rare | rare | possible | likely | almost_certain`.
  - `status` ∈ `unmitigated | partially_mitigated | mitigated | risk_accepted`.
  - `controls`: current mitigations, or `none`.
  - `evidence`: past CVEs, kanban issue ids (`native:abc…`), git
    commits. **Evidence raises likelihood; it is not the threat.**

  Sort by (impact, likelihood) descending.

- **5. Deprioritized** — table `| threat | reason |`.
- **6. Open questions** — bullet list of what the mode could not
  determine.
- **7. Provenance** — `mode | date | target | inputs | owner`.
- **8. Recommended mitigations** — table
  `| mitigation | threat_ids | closes_class | effort |` where
  `closes_class ∈ {yes, partial}` and `effort ∈ {S, M, L}`. Each row
  is **one class-level control**, not a per-finding patch.

## Mapping threats → Seki findings

Threats from section 4 anchor how Seki labels findings on the
kanban (cf. `[[iterion-board]]`):

- A finding whose root cause matches a threat row inherits the
  threat's `id` in its issue body ("Instance of T3"), and the
  revalidate judge may bump severity ONE step when the match is
  unambiguous (never two — keeps the precondition rule honest).
- Threats with no instances yet still seed scanner focus and may
  become `triage-uncertain` issues with `type:<finding-type>` and
  `source:sec-audit-source` labels for human review.
- Twelve finding categories live in `[[finding-taxonomy]]`; threats
  reference them by category name in `surface` or `evidence` notes.

## Modes

### `interview` — owner is present

Walk the owner through Shostack's four questions:

| Q | Question | Fills |
|---|---|---|
| Q1 | What are we working on? | sections 1, 2, 3 |
| Q2 | What can go wrong? | section 4 (id, threat, actor, surface, asset) |
| Q3 | What are we going to do about it? | section 4 (impact, likelihood, controls), 5, 8 |
| Q4 | Did we do a good job? | section 6, coverage check |

Use AskUserQuestion in small batches; record answers as you go;
ground claims in code where possible. Owner statements that can't
be verified in code go to section 6.

### `bootstrap` — no owner

Five stages, run in parallel where you can:

1. **Recon.** Read top-level layout, `README`, route registrations,
   middleware setup, package manifests. Output: candidate entry
   points and assets.
2. **Surface mapping.** Spawn parallel readers per subsystem
   (auth, data, network handlers, crypto primitives, infra config).
3. **Past-vuln mining.** Read `git log`, `CHANGELOG`, GitHub
   Security Advisories. Each past vuln becomes evidence on a threat.
4. **Generalize.** Group vulns into threat classes (don't list
   each CVE — list the class CVEs instantiate).
5. **STRIDE gap-fill.** For each entry point, check spoofing /
   tampering / repudiation / info-disclosure / DoS / elevation;
   add rows for plausible threats with no instances yet, marked
   `status: unmitigated`, `controls: none`, `evidence: (none)`.

### `bootstrap-then-interview` — owner available but time-limited

Run bootstrap unattended, then walk the owner through section 6's
open questions and any thin threat rows. Owner time goes to
**refining** a code-grounded draft, not building from scratch.

## Safety

- **Static only.** No build, no execution, no network calls against
  the target's infrastructure. If asked to validate a threat by
  running an exploit, decline — that's downstream tooling's job.
- **Public advisory DBs only** when fetching CVEs (NVD, GHSA, the
  project's own issue tracker). Never the live deployment.
- Bootstrap is idempotent. Re-running it overwrites
  `.sec-audit/context.md`. Operators may edit the file by
  hand between runs.

## After writing

Print:
1. Path to `.sec-audit/context.md`.
2. Top 5 threats by `impact × likelihood` (id, one-line, score).
3. For `bootstrap`: open questions (these seed a later interview).
4. For `interview`: owner claims unverifiable in code.

## See also

- `[[finding-taxonomy]]` — the twelve categories threats reference.
- `[[iterion-board]]` — kanban shape that consumes ranked findings.
- `[[sec-audit-source]]` — Seki's six-phase playbook that reads
  `context.md` during triage and revalidate.
- `HARNESS-ATTRIBUTION.md` — upstream credit.
