# ADR-059 — Skill library: a first-class, referenceable store of skills

Status: **accepted** (2026-07-05).

## Context

Skills (Claude Code `SKILL.md` markdown files) exist in iterion today **only**
coupled to a distributing artifact:

- **bundle skills** — `<bundle>/skills/*.md`, shipped with and scoped to one bot
  (`pkg/runtime/bundle.go` mirrors them into `<workspace>/.claude/skills/`);
- **plugin skills** — `contributes.skills` in a `plugin.yaml`; a bare `skills/`
  git repo is even auto-wrapped as a disabled-by-default "skills-only plugin"
  (`plugin.SynthesizeSkillsManifest`, `pkg/plugin/skilllib.go`) and mirrored by
  `pkg/runtime/plugin_skills.go`.

Both sources converge on **one** mount point — `<workspace>/.claude/skills/` —
read by claude_code (`--setting-sources project`) and by claw's `skill` tool
(`skillLookupRoots`). There is **no standalone library**: nowhere to author/curate
skills independently of a bundle, and no way for a workflow to **reference** a
specific skill by name. The closest primitive, the synthesized skills-only
plugin, is an all-or-nothing *import a git repo* unit with an install/enable
lifecycle — the wrong shape for "a library the operator edits skill-by-skill and
a workflow cherry-picks from".

## Decision

Introduce a first-class **skill library** with two coordinated halves:

1. **A dedicated store** (`pkg/skilllib`), modeled on the local sealed-secret
   store (`pkg/secrets/local.go`): global `~/.iterion/skills/<name>/SKILL.md`
   with a per-project override `<workDir>/.iterion/skills/<name>/SKILL.md`
   (directory form, flat `<name>.md` fallback). Per-skill CRUD; project shadows
   global by name. This is the **hybride** model: the dedicated store holds
   hand-authored/edited skills, and the existing plugin path
   (`SynthesizeSkillsManifest`) is retained as the *third-party pack import* route,
   surfaced as `iterion skill import <git-url>`.

2. **A DSL reference** — a `skills: [name, ...]` field on agent/judge nodes and
   as a workflow-level default (modeled exactly on `capabilities:`). At run start
   the runtime resolves the union of referenced names against the library and
   mirrors **only those** into `.claude/skills/` (reusing the existing 4-branch
   `reconcileSkillFile` collision policy), and injects a `## Skills` hint
   (name + description, referenced-by-the-node only) into that node's system
   prompt. The skill body is loaded on demand by the agent from `.claude/skills/`,
   not inlined.

Reuse, don't duplicate: the frontmatter parser (`runview.readSkillFile`) is
extracted into a shared helper; the mirror engine (`runtime.reconcileSkillFile`)
is called as-is.

### Resolved sub-decisions

- **Unknown-reference validation** — a `skills:` entry not found in the library
  emits **`C199` (DiagUnknownSkillRef)** as a **warning**, resolved against the
  library on disk. Never an error: compiles stay portable (CI without the library
  passes; the run simply doesn't mirror the missing skill). This differs from the
  hermetic `needs:`/`resources:` C195 error model on purpose — the library is
  machine-local state, not part of the `.bot`.
- **Name-collision precedence** — `bundle > plugin > library > hand-authored`.
  Because `reconcileSkillFile` keys on the destination path + its sha256 marker,
  whichever mirrors **first** into a name wins and later sources observe "shadow".
  Call order is therefore bundle → plugin → library; an unmanaged file the
  operator placed by hand (no marker) shadows all three.
- **Scope** — `iterion skill add` defaults to **global**; `--scope project`
  writes the per-project override, which fully shadows the global of the same
  name (identical to the secret store's layered semantics).

## Consequences

- A new cross-cutting primitive touching store ↔ DSL ↔ runtime ↔ CLI ↔ server ↔
  studio. Surfaces: `iterion skill {list,show,add,rm,import,export}`,
  `GET/POST/PATCH/DELETE /api/local/skills` (gated on
  `server_info.skills_enabled`, non-cloud only), a studio **Skills** view (CRUD +
  markdown editor), and the `skills:` DSL field.
- The library needs no sealing (skills are not secrets), so `skills_enabled`
  gates on local mode alone — simpler than the secret store's sealer gate.
- Precedence is a mirror-ordering property, not a lookup-time merge — the shadow
  outcome is logged, so a collision is observable rather than silent.
- The plugin skills-only-import path and the library coexist by design: plugin =
  shared/versioned bundle-of-skills unit; library = personal editable store.
  `iterion skill import` bridges them.
- Cloud mode is out of scope for now (the store is local-only, like local
  secrets); a cloud/team-shared library is a future extension along the same seam.
