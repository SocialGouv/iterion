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
      text: Get started →
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
    details: Mix providers and backends per node — a plan from one model reviewed by another. Claude Code, in-process claw, Kimi, and Grok, with credential auto-detection.
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

New here? Start with [Why Iterion?](/why-iterion) for the vision, or jump
straight to [Install](/install) and [the bot fleet](/examples). The
[current as-built state](/current-state) is the honest, verified-against-`main`
picture of what ships today.
