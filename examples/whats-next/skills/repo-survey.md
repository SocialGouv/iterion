---
name: repo-survey
description: Survey checklist for the whats-next.bot `explore` phase — produces the structured explore_output that the propose phase consumes.
---

# Repo Survey — for whats-next.bot's `explore` phase

Use this skill at the start of the `explore` phase. Your job is to
produce a structured `explore_output` object that the downstream
`propose_roadmap` agent (also claw + GPT-5.5) will consume to draft
a roadmap recommendation. Every field below maps directly to that
schema:

```
explore_output:
  summary:        string    # 5–10 lines, narrative
  toplevel_dirs:  json      # array of {name, role}
  recent_commits: json      # array of {hash, line, theme}
  open_questions: json      # array of short strings — see §7
  observations:   string    # 3–6 lines of judgment calls
```

## Phase contract

You have these tools and **only** these tools: `bash`, `read_file`,
`glob`, `grep`. `bash` is restricted to read-only inspection (see
the system prompt's bash allowlist). `readonly: true` is set at the
node level; mutations are runtime-blocked even if your prompt
appears to ask for one.

Budget: ~25 tool calls total. Go deep on what matters, skip the
rest.

## 1. Map the top level → `toplevel_dirs`

```bash
find "$WORKSPACE" -maxdepth 1 -mindepth 1 -type d -printf '%f\n' | sort
```

Emit each entry as `{name, role}`. Roles to use:
`code | tests | docs | tooling | examples | infra | vendored |
runtime-data | unknown`.

Classify hidden dirs only when relevant (`.iterion/` = runtime-data,
`.github/` = infra, `.claude/` = tooling — see §6 about
`.claude/skills/`).

## 2. Read the convention files → `summary` line 1–2

Always read (if present): `README.md`, `CLAUDE.md`,
`CONTRIBUTING.md`. Capture in `summary`:
- Product framing (1 sentence)
- Build / test entry point (e.g. `devbox run -- task build`)
- The `.iter`/`.bot` distinction if iterion

## 3. Identify the stack → `summary` line 3–4

```bash
find "$WORKSPACE" -maxdepth 2 \
  \( -name 'Taskfile.yml' -o -name 'devbox.json' -o -name 'go.mod' \
     -o -name 'package.json' -o -name 'Cargo.toml' \
     -o -name 'pyproject.toml' \) -printf '%P\n'
```

State the primary language(s) and package manager(s) so
`propose_roadmap` knows which bot ecosystem fits.

## 4. Discover bots → `summary` line 5 + downstream signal

This is **the** field `propose_roadmap` needs most. List every
`.bot` / `.iter` / `.botz` file in the workspace:

```bash
find "$WORKSPACE" -maxdepth 4 \
  \( -name '*.bot' -o -name '*.iter' -o -name '*.botz' \) \
  -not -path '*/vendor/*' -not -path '*/.iterion/*' \
  -printf '%P\n' | sort
```

Record the path of each bot in `summary` (NOT just names — paths,
because `propose_roadmap.next_action.bot_path` will need them).
If the repo is not iterion, this may be empty — that's a valid
signal.

## 5. Recent activity → `recent_commits`

```bash
git -C "$WORKSPACE" log -n 20 --oneline
```

For each commit, emit `{hash, line, theme}`. Themes are
free-text but stick to a small vocabulary you reuse across
commits (e.g. "editor", "dispatcher", "dsl", "test", "docs",
"runtime", "sandbox"). The propose phase will use `theme`
frequencies to spot what's hot.

## 6. ADRs and architectural intent → `observations`

```bash
find "$WORKSPACE/docs" -maxdepth 3 \
  \( -name 'adr*' -o -name 'architecture*' \) -type f \
  -printf '%P\n'
```

If there are accepted ADRs, note them. The propose phase MUST NOT
recommend work that contradicts them; surface any tension you
spot in `observations`.

## 7. Open questions → `open_questions`

These are the things YOU want the operator to clarify before
`propose_roadmap` runs. The downstream `ask_priorities` human
node will surface them. Examples of good open_questions:

- "Recent commits are heavy on dispatcher work — is that still
  the priority, or has it shipped?"
- "I see both editor TODO markers and runtime TODOs — which area
  is more painful right now?"
- "Stack is multi-language (Go + React + Helm). Which subsystem
  should the next action focus on?"

Examples of bad open_questions (don't emit these):
- "What do you want to do?" (too broad — that's what
  ask_priorities already asks)
- "Should I run feature_dev or whole_improve_loop?"
  (premature — the propose phase decides this, not you)

Keep open_questions to 0–4 items. Empty is fine if the survey
makes the priorities obvious.

## 7b. Findings inbox → `observations` (dispatcher bot handoff)

Other dispatcher-spawned bots write **findings** — observations
they noticed during their own runs but did not treat — into the
project-rooted memory directory:

```
~/.iterion/projects/<encoded-repo-root>/memory/findings/
```

The system prompt's `vars.findings_dir` already resolves to the
correct absolute path for this run. The directory may not exist
yet (fresh project / no bot has written one yet):

```bash
ls "$FINDINGS_DIR" 2>/dev/null
```

`2>/dev/null` silences the missing-dir error and is explicitly
allowed by the system prompt's Constraints exception. Do NOT run
`mkdir` — the directory is created when a bot first writes a
finding via its native Write tool.

For each `.md` file listed, call `read_file` to see its
frontmatter (`title`, `description`, `kind`, `source_bot`,
`tags`) plus the first ~40 lines of body. Don't burn context on
full bodies unless one looks high-priority or you suspect a
duplicate is queued.

Fold the catalogue into `observations` as bullet lines of the
form:

```
[finding:<source_bot>:<kind>] <file_basename> — <title>
```

…where `<file_basename>` is the `.md` filename only (no path
prefix). The exact basename matters: `emit_action` needs it to
`rm` the file after a kanban issue ingests the finding (transient
queue lifecycle — bots that wrote unmatched findings re-surface
them next session).

Empty `findings/` directory → no bullets. Do NOT manufacture
findings.

Do NOT delete or modify finding files in this node. Read + report
only; the lifecycle flip happens in `emit_action`.

## 8. Don't drown in TODO/FIXME

Scan first-party only:

```bash
grep -RInE 'TODO|FIXME' --include='*.go' --include='*.ts' \
  --include='*.tsx' --include='*.py' --include='*.rs' \
  --exclude-dir=vendor --exclude-dir=node_modules \
  "$WORKSPACE" | head -50
```

Note hotspots in `observations`, not every individual TODO.

## 9. What you do NOT do

- You do NOT propose a roadmap. That's `propose_roadmap`'s job.
- You do NOT pick a `bot_to_run`. That's also `propose_roadmap`.
- You do NOT read the operator's mind. Use `open_questions`
  instead.
- You do NOT modify any file. `readonly: true` plus the bash
  allowlist enforce this — your job is observation only.
