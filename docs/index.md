---
layout: home

hero:
  name: Iterion
  text: The control plane for AI agents.
  tagline: Apps have Linux. The cloud has Kubernetes. AI agents have Iterion.
  image:
    src: /iterion-logo.png
    alt: Iterion logo
  actions:
    - theme: brand
      text: Why Iterion?
      link: /why-iterion
    - theme: alt
      text: Install
      link: /install
    - theme: alt
      text: Current state
      link: /current-state

features:
  - title: Start here
    details: As-built overview, install paths, examples, the CLI map, and the browser studio.
    link: /current-state
  - title: Author .bot workflows
    details: The DSL guide, grammar, routers, human gates, cursors, supervisors, backends, permissions, and authoring pitfalls.
    link: /dsl
  - title: Run and operate locally
    details: Resume, sandbox, scheduling, the dispatcher, native tracker, settings precedence, and persisted formats.
    link: /bot-invocations
  - title: Bots & security automation
    details: The maintained bot catalogue and the source/dependency security-audit bots.
    link: /examples
  - title: Cloud / agent control plane
    details: The event → queued run → result-posted-back loop, forge integrations, webhooks, quotas, and the REST/remote-CLI surface.
    link: /cloud-overview
  - title: Desktop
    details: The Wails desktop app, its architecture, reproducible builds, and release QA.
    link: /desktop
  - title: Architecture & contribution
    details: The end-to-end compiler → runtime → backend → persistence architecture and the reproducible toolchain.
    link: /architecture
  - title: Point-in-time records
    details: ADRs, dated dogfood bilans, plans, reviews, and security audits — evidence, not living references. Browse them from the sidebar.
    link: /bot-runs/README
---

Guides and references above are maintained against `main`. ADRs, dated plans,
audits, reviews, and bot-run bilans are point-in-time records that may
intentionally describe an older state — they do not override current code or
the living references.
