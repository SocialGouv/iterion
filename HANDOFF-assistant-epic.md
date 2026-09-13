# Handoff — `feat/assistant-epic` (studio assistant dock + skills)

**Branch:** `feat/assistant-epic`, pushed, head `d44102bc`.
**Worktree:** `.works/assistant-epic` — run everything from there, never from
the main checkout.
**Operator:** Victor. Writes in French; answer in French.

Read `CLAUDE.md` first — it is authoritative and this file does not repeat it.

---

## 1. What this branch is

Two threads that ended up braided:

1. **The assistant dock** — the studio's shell-level chat, reachable on every
   route, answered by **Copi** (`bots/copilot`). Reference:
   [`docs/assistant-dock.md`](docs/assistant-dock.md), ADR-091.
2. **Skills** — what Copi knows, and (new) how an operator adds their own to
   any run. Reference: [`docs/skills-library.md`](docs/skills-library.md).

### Commits on top of `a1fe0109` (the operator's own)

| | |
|---|---|
| `b4da3217` | one bubble shape for every operator turn; `[page context:]` stripped from DISPLAY only |
| `68d396b2` | floating panel bounded to the viewport; re-clamps on window resize |
| `383d623d` | conversation tabs, "way back" link, context strip → composer eye |
| `42de0532` | `--skill` / `ITERION_SKILLS`: add a library skill to any run |
| `d44102bc` | `iterion-bot-architecture` skill for Copi + catalog skill-reference test |

**Three `wip(iterion): auto-banked …` commits sit underneath** (`752f76a6`,
`ec7257d7`, `e338a7c9`). Iterion runs banked uncommitted work mid-session.
They are unreviewed with generated messages. The branch squash-merges through
the merge queue so they vanish at merge; rewrite them only if the operator
asks for a clean history before the PR.

---

## 2. The doctrine — do not re-litigate these

These are the operator's decisions, arrived at over the session. Treat them as
settled; they explain most of the code.

**On the assistant:**
- **Propose a page change with a LINK, never a violent switch.** Copi invites,
  the operator clicks, and *the click is what answers the paused turn*
  (`useEditorConsent` waits for the route to settle before sending).
- **Copi acts only on the currently open page.** Hence the design posture's
  ordering: orient → invite → build.
- **Copi builds visibly**, in real time, on the canvas the operator is
  watching.
- **The bot writes the sentence introducing a button; the studio only renders
  the button.** No static English caption from the UI.

**On naming:** two chat-shaped surfaces coexist on `/runs/:id` and do opposite
things. **Assistant** — you ask, it answers. **Steering** — you push into a
LIVE agent's inbox, nothing replies. Both were once titled "Conversation";
that ambiguity is why `studio/src/lib/chatDock/labels.ts` exists. Keep them
named apart.

**On configuration, three rules that keep recurring:**
- **Hint, not filter.** Preset `Skills` and the run-level `--skill` list make
  a skill *relevant*; they never remove one. Filtering by a bot's posture was
  proposed and rejected: a posture is a bias its own turn may flip, so a
  filter locks the agent out exactly when it discovers it needs the skill.
- **Additive, not replacing.** Operator input unions with the author's.
- **Say what is news; be silent about what the operator can already see.**
  This is why the "Looking at <page>" strip was retired — and why it still
  renders for `dismissed` and `degraded`.
- **A value the operator TYPED that resolves to nothing is an error**, listing
  what is available. A workflow's own reference stays soft. Nobody typed those.

---

## 3. Traps that cost this session real time

**Building the studio takes THREE steps, not one.** `pkg/server` embeds
`pkg/server/static`, not `studio/dist`:

```sh
cd studio && npx vite build
rm -rf ../pkg/server/static/assets ../pkg/server/static/index.html
mkdir -p ../pkg/server/static && cp -R dist/. ../pkg/server/static
cd .. && CGO_ENABLED=0 go build -o ./iterion ./cmd/iterion
```

Skipping the copy serves the OLD bundle and looks like your change did
nothing. (`task studio:dev` does this for you; a hand-built binary does not.)

**Bundle skills get NO generated `## Skills` roster.** `SetSkillHints` is fed
only by `applyLibrarySkills` — the skill LIBRARY (`~/.iterion/skills`) and
plugin contributions. A bundle skill is mirrored to disk but produces no hint,
and the `skill` tool's schema enumerates nothing. **So a bundle skill must be
NAMED in the bot's own system prompt or the model never learns it exists.**
`bots/catalog_skill_refs_test.go` pins both halves.

**`##` is the DSL comment marker.** The lexer eats the line; the model never
sees it. A skill "documented" in a comment beside its declaration is invisible.

**Copi's every turn is a resume.** It pauses at a human node per turn and the
dock drives one resume per message. Anything applied only at launch is gone by
the operator's second reply — that is why `run.ExtraSkills` is persisted.

**`pkill -f 'iterion studio --port 4899'` kills your own shell** (the pattern
matches the bash tool's own command line, exit 144). Use `'iterion stud[i]o'`.

**`read -t 1 </dev/null` does not sleep** — it returns instantly at EOF, so a
poll loop built on it runs in zero time and reports a false negative. Use
`curl -sf --retry N --retry-delay 1 --retry-connrefused`.

**The studio takes ~40 s to boot** in this store, finalizing old worktrees
before it binds the port. It is not hung.

**Browser automation cannot verify a real drag.** `left_click_drag` fires
pointermove without pointerdown (measured). Resize handles must be verified by
the operator with a real mouse — say so rather than claiming it works.

**`pkg/dsl/ir/compile.go` fails `gofmt`.** Pre-existing, already on `main`,
deliberately untouched here to keep the diff clean. It will redden `task lint`.

---

## 4. Verify before you claim anything

```sh
cd .works/assistant-epic
go build ./... && go vet ./pkg/... 
go test ./pkg/runtime/ ./pkg/cli/ ./pkg/store/ ./pkg/runview/ ./bots/
go test ./e2e/                     # ~3 min
./iterion validate bots/copilot    # expect only C128 (deliberate sandbox opt-out)
cd studio && npx tsc --noEmit && npx vitest run   # 1565 tests
```

**Mutation-test every new test.** Five defects this session were found by the
operator's manual testing and none by tests that "passed" — including one of
mine that passed against a deleted prompt roster because a `##` comment still
mentioned the skill. Break the thing, watch the test fail, restore.

The operator's studio runs on **`http://localhost:4899`** from this worktree's
binary. Live dogfood runs must land in a store the operator can watch — never
a throwaway `--store-dir /tmp/...`.

---

## 5. Open work

### Committed to, not done — the cloud half of `--skill`
Cloud runs cannot carry operator-added skills. The engine, persistence and
resume are done; the missing piece is the **surface** that would set them
(studio Launch modal field / HTTP API `LaunchSpec.Skills`). Deliberately not
half-plumbed — a cloud path nothing can feed is dead code.

When that surface lands, the cloud half is **one union**: the run's extras
join `collectWorkflowSkillRefs` in
[`pkg/server/cloudpublisher/contributions.go`](pkg/server/cloudpublisher/contributions.go).
That payload already ships each library skill's **content**
(`queue.LibrarySkillFile`), because a runner pod has no library on disk. Note
`spawnRun` threads `preset` as a positional parameter through several layers —
`launchExtras` already carries `extraSkills`, so extend that, not the
signature.

### Small, clearly worth doing
- **The composer's skill picker is dishonest.** `WithMessageSkills` is
  documented "Sticky — the skill stays loaded for the rest of the run", and
  the picker says nothing. Either say so, or make it truly per-message.
- **`iterion skill add` accepts a file with no frontmatter silently.** The
  operator's own standard installed with an EMPTY description — and the
  description *is* the roster line. Warn at add time.
- The operator still needs to add frontmatter to
  `~/Workspace/agent-standards/iterion/bot-authoring.md`. **Do not edit that
  file** — it lives outside the repo and is theirs.

### Offered during the session, never answered — ask before building
Per-tab unread badges · the tab strip showing workplace instead of origin ·
removing conversational-bot runs from the `/pipelines` projection · typed
graph ops for true node-by-node canvas building · a UI way to re-attach an
orphaned run to a conversation · making `dedupeRunIds`' repair announce itself
instead of silently dispossessing a tab.

### Explicitly rejected — do not build without re-opening the argument
- **Skills configured per posture** (info/design/debug). See the doctrine
  above: a posture is a bias, filtering by it is a wall in the wrong place.
- **A settings panel redefining a bot's skill list.** It moves the contract
  from the bot's author to invisible per-user state, and makes bug reports
  irreproducible.

### Abandoned elsewhere
**PR 477** (`fix/worktree-pool-bound`) was the session's original task and was
dropped when the operator pivoted. CI (`test`, `race`, `cloud-e2e`) and an
11th Revi pass were in flight. Unrelated to this branch.

---

## 6. Where things live

| What | Where |
|---|---|
| Dock shell, three states, resize | `studio/src/components/ChatDock/ChatDockShell.tsx` |
| Dock body, composer, context eye | `studio/src/components/ChatDock/ChatDock.tsx` |
| Conversation tabs + workplace link | `ConversationStrip.tsx`, `lib/chatDock/conversations.ts` |
| Page-context strip / eye | `ContextChip.tsx` (`stripSpeaks`), `ContextEye.tsx` |
| Route → typed reference | `lib/chatDock/routeReference.ts` (`orView` marks `degraded`) |
| Assistant vs Steering naming | `lib/chatDock/labels.ts` |
| Run-console steering panel | `studio/src/components/Runs/FloatingChatPanel.tsx` |
| Copi | `bots/copilot/main.bot` + `bots/copilot/skills/` (5 skills) |
| Library-skill mirror + union | `pkg/runtime/library_skills.go` |
| `--skill` / `ITERION_SKILLS` | `pkg/cli/extra_skills.go` |
| Persisted list | `store.Run.ExtraSkills`, event `skills_injected` |

**One thing that reads as a regression and is not:** the run page's
"CONVERSATION" window still exists — renamed **Steering**, moved one lane left
of the assistant, same file and same persisted key (`run-console-v2.chat-dock`).
It is the only surface carrying a run's transcript and its human-pause form,
so it is not replaceable by the assistant.
