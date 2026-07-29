---
layout: home

hero:
  name: Iterion
  text: The control plane for AI agents.
  tagline: Apps have Linux. The cloud has Kubernetes. AI agents have Iterion. Define agent workflows as readable .bot files and operate every run — locally, in CI, or across a multi-tenant cloud.
  image:
    src: /iterion-logo.png
    alt: Iterion logo
  actions:
    - theme: brand
      text: Quickstart →
      link: /quickstart
    - theme: alt
      text: Install & all modes
      link: /install
    - theme: alt
      text: Explore the bot fleet
      link: /examples
    - theme: alt
      text: Why Iterion?
      link: /why-iterion

features:
  - icon: 🧩
    title: Declarative .bot workflows
    details: Chain agents, judges, routers, human gates, tools, and compute into one readable graph — versioned, diffable, re-runnable without an interpreter.
    link: /dsl
    linkText: Learn the DSL
  - icon: 🤝
    title: Multi-agent, multi-model
    details: Mix providers and backends per node — a plan from one model reviewed by another. Claude Code, in-process claw, `pi`, Kimi, and Grok, with credential auto-detection.
    link: /backends
    linkText: Backends & routing
  - icon: 🛡️
    title: Safe autonomy
    details: Per-run Docker/Podman/Kubernetes sandboxes, a tool-permission gate, sealed secrets, and shared budget caps on tokens, cost, and time — on by default.
    link: /sandbox
    linkText: Sandbox & policy
  - icon: ⏯️
    title: Durable, resumable runs
    details: A run can pause for a human or fail at minute 80 and resume from minute 75 under the same ID — no replay, no lost work, whether the pause lasted seconds or days.
    link: /resume
    linkText: Resume & recovery
  - icon: 📈
    title: Measure quality — the asymptote
    details: Run a workflow ten times, plot the quality, watch it converge. Turn "is this good enough to trust unattended?" into a number instead of a vibe.
    link: /asymptote-bench
    linkText: Asymptote bench
  - icon: 🖥️
    title: Run it your way
    details: One Go core, delivered everywhere — a scriptable CLI, a visual studio, a native desktop app, Docker, CI, an autonomous dispatcher, and a multi-tenant cloud.
    link: /install
    linkText: All operation modes
  - icon: 🤖
    title: A production bot fleet
    details: A co-CTO, a feature builder, source & dependency security auditors, a docs aligner, a dependency upgrader — each a declarative bot you can run, inspect, or fork.
    link: /examples
    linkText: Browse the bots
  - icon: ☁️
    title: A cloud control plane
    details: Orgs and teams, quotas, bound credentials, inbound webhooks, an audit trail, and an always-on run engine — the same workflows, operated at team scale.
    link: /cloud-overview
    linkText: Iterion Cloud
---

## ✨ See it

A `.bot` is a readable graph. Here's a plan → implement → review loop that keeps
fixing until a judge approves — declarative, versioned, re-runnable:

```iter
schema verdict:
  approved: bool
  notes: string

agent plan:
  system: "Turn the request into a concrete plan: files, steps, constraints."

agent implement:
  backend: "claude_code"
  system: "Implement the plan, then run the tests."

judge review:
  output: verdict
  system: "Approve only if the change is complete and the tests pass."

workflow ship_it:
  entry: plan
  plan -> implement -> review
  review -> implement as fix(3) when not approved
  review -> done when approved
```

… which compiles to this graph and runs it — in parallel where it can, with
budgets, sandboxing, and checkpoints:

```mermaid
flowchart LR
  P(["📝 plan"]) --> I["🤖 implement"] --> R{"⚖️ review"}
  R -->|"✅ approved"| D(["🏁 done"])
  R -->|"🔁 fix · max 3"| I
```

Design it on the canvas or edit the source — both stay in sync in the
[visual studio](/visual-editor):

![The Iterion studio — visual workflow editor with live diagnostics and a per-node inspector](images/studio/editor-canvas.png)

## ⚙️ How a run works

Launch from anywhere; the same compiled graph runs the same way — scheduled in
parallel, bounded by budgets, isolated in a sandbox, and checkpointed so it can
resume:

```mermaid
flowchart LR
  L(["🚀 CLI · studio · CI · cloud"]) --> C["🧩 Compile + validate<br/>.bot → graph"]
  C --> E{{"⚙️ Runtime engine<br/>schedule · budget · checkpoint"}}
  E --> N["🤖 agents · judges · routers<br/>humans · tools · sub-bots"]
  N --> S[["🛡️ Per-run sandbox<br/>worktree · secrets · policy"]]
  S --> R(["📦 PRs · artifacts · event log"])
```

## 💡 What teams build with it

Iterion ships a maintained fleet of **25+ production bots** — real, grounded
examples of what an operated agent workflow looks like. A few of the jobs they do:

| | Use case | Bots that run it |
|---|---|---|
| 🚀 | **Ship software from a prompt** — greenfield apps and end-to-end features | Appy · Featurly · Fini · Bmady |
| 🔁 | **Continuously review & improve a codebase** — whole-repo and per-branch campaigns, cross-family PR review, test coverage | Willy · Billy · Revi · Testy |
| 🛡️ | **Automated security & supply chain** — SAST, dependency/SCA, diff-scoped CVE & malware shields on every PR | Seki · Depsy · Vulny · Shieldy |
| ⬆️ | **Upgrade dependencies safely** — multi-stack agentic upgrades with a review gate | Renovacy · Vetty |
| 📚 | **Keep docs & knowledge aligned** — docs alignment, wiki generation, ADR cartography | Doki · Wikky · Adry |
| ♿ | **Accessibility audits** — RGAA 4.1.2 over deterministic gates | Acci |
| 🗂️ | **Triage & route work** — classify board cards, auto-open PRs, answer PR questions | Triagy · Revi |
| 📡 | **Watch & react** — feed/veille monitoring and digests | Vigie |
| 🧭 | **A strategic partner** — a conversational co-CTO and roadmap, an architectural visionary | Nexie · Evoly |

[Browse the full catalogue →](/examples)

## 🧰 A catalogue you run — or make your own

Run any bot as-is, **fork and adapt** one to your repo, or **author your own**
from scratch — in the [visual studio builder](/visual-editor), with `iterion
bots create`, or by hand in the readable [`.bot` DSL](/dsl). Package it as a
`.botz` bundle and share it through the [marketplace](/plugins). The same
workflow runs unchanged from your laptop to CI to the cloud.

## 🏗️ Built for real engineering

<div class="vp-features-lite">

- 🔗 **Forge-native** — GitHub, GitLab & Forgejo: open and review PRs, answer PR/MR comments, trigger on webhooks, triage issues, and post results back. → [Forge integrations](/forge-integrations)
- 🔐 **Secrets, done right** — a sealed local & cloud secret store, bring-your-own provider keys, per-run sealed bundles, egress-scoped file secrets — never printed, materialised only at execution sinks. → [Secrets](/secrets)
- 🧱 **Safe by default** — per-run Docker/K8s sandboxes, a tool-permission gate against prompt injection, shared budget caps, and worktree isolation with an explicit merge policy. → [Sandbox](/sandbox)
- ⚡ **Event-driven** — an issue-tracker [dispatcher](/dispatcher), cron [schedules](/scheduling), and a trigger spine over webhooks, board events, and run-completion chains.
- 👁️ **Operable** — steer live runs, attach [supervisors](/supervisors) that watch and correct an agent, async human gates, checkpoint/[resume](/resume), and Prometheus/OTLP/Grafana observability.
- ☁️ **Team scale** — orgs & teams, quotas, audit, SSO, PATs, and a remote CLI — the same engine from laptop to [multi-tenant cloud](/cloud-overview).

</div>

---

New here? Start with [Why Iterion?](/why-iterion) for the vision, jump straight
to [Install](/install), or [explore the bot fleet](/examples).
