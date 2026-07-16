---
name: nextjs-dsfr
display_name: Next.js + DSFR
description: Application Next.js (App Router, TypeScript) avec le Système de Design de l'État (DSFR) — RGAA 4.1.2 par construction
vars:
  stack: "nextjs-dsfr"
skills: [rgaa-dsfr]
---
Bias the scaffold and every slice toward a Next.js (App Router,
TypeScript) application integrating the Système de Design de l'État
via `@codegouvfr/react-dsfr`, installed and wired per that package's
official documentation (DsfrHead / provider setup, fonts and icons
assets, color scheme handling). Prefer the DSFR's own components and
`fr-*` utility classes (Header, Footer, navigation, forms, cards) over
hand-rolled equivalents, semantic HTML over ARIA patches, and
French-first UI text.

RGAA 4.1.2 / WCAG 2.1 AA conformance is part of the definition of done,
not a later pass: read the `rgaa-dsfr` skill and compare against the
official accessible markup (the DSFR MCP tools when available) whenever
you author or modify a UI surface — landmarks, heading hierarchy,
labelled controls, keyboard operability, visible focus, contrast.

Walking skeleton for this preset: one accessible home page rendered
with the official DSFR Header + Footer + a main landmark, one
`GET /api/healthz` route returning 200, and one smoke test that renders
the home page and asserts the DSFR Header is present. First-draft
definition of done adds: the production build passes and the dev server
serves the home page with the DSFR styles actually loaded (not a bare
unstyled page).
