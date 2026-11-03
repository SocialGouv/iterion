# ADR-160: Legal content is deployment configuration — never the bundle, never the database

- **Status**: Accepted — the operator picked this option on 2026-11-03 (écart 9 of the DA↔repository review, option A: « config de déploiement (values → ConfigMap), déclaration “non conforme” d'abord »); this record is the ADR, and the change that carries it is the implementation.
- **Date**: 2026-11-03
- **Authors**: devthejo (session with Claude), for écart 9 (legal pages: legal notice, privacy policy, terms of use, accessibility statement)
- **Landed by this change**: [`pkg/legal`](../../pkg/legal/legal.go) (the slug table, the loader, the accessibility declaration), the `legal` family of [`pkg/config`](../../pkg/config/config.go) (`ITERION_LEGAL_*`), `GET /api/legal/{slug}` and the `legal` block of `/api/server/info` ([`pkg/server/legal_routes.go`](../../pkg/server/legal_routes.go)), the studio pages under `/legal/<slug>` with their links (product home, sign-in card, sidebar, public catalogue, sitemap), `config.legal.*` in the chart (a rendered `<release>-legal` ConfigMap mounted read-only), the templates in [`docs/legal-templates/`](../legal-templates/). Witnesses: `pkg/legal/legal_test.go`, `pkg/config/config_test.go` (`TestLoad_Legal*`), `cmd/iterion/server_legal_test.go`, `pkg/server/legal_routes_test.go`, `studio/src/views/Legal/Legal.test.tsx`, `studio/src/views/Legal/legalShells.test.tsx`, `studio/src/__tests__/legalDoor.test.tsx`, `charts/iterion/render_test.go` (`TestLegalPagesAreDeploymentConfiguration`).
- **Relates to**: [url-layout.md](../url-layout.md) (what answers at the root) · [cloud-public-exposure-checklist.md](../cloud-public-exposure-checklist.md) (the privacy items these pages tick) · [090-runtime-operational-settings-db-backed.md](090-runtime-operational-settings-db-backed.md) (the DB-backed settings ladder this record deliberately stays OUT of)

## Context

A public online service in France owes its visitors four texts: a legal
notice (LCEN art. 6-III — publisher, publication director, hosting
provider), the information due to data subjects (RGPD art. 13 —
controller, purposes, legal bases, recipients and processors, transfers,
retention, rights, DPO), terms of use, and an accessibility statement
(loi 2005-102 art. 47, décret 2019-768 — conformance status, date, rate
when a full audit exists, non-accessible content, feedback contact,
recourse). iterion.cloud publishes none of them; the RGAA audit of the
studio names their absence (contact, legal notice, accessibility
statement among the page kinds the studio lacks), and the public-exposure
checklist's privacy items cannot be ticked.

The texts themselves are facts about ONE operator: a ministry's
publisher, its DPO, OVH's address, a conformance rate measured on one
deployment. The product is MIT code whose single studio bundle serves
iterion.cloud, a preprod host and every self-hosted deployment
([url-layout.md](../url-layout.md): the same `index.html` is served
everywhere, which is why `og:image` is absolutised at runtime rather than
at build time). So the question was not whether to publish the pages, but
WHERE the content lives.

## Options considered

- *A — deployment configuration.* Values → a ConfigMap of markdown files
  mounted on the server pod + three env vars (contact, accessibility
  status, date/rate), read once at startup, served public, linked only
  when configured. Same pattern as `ITERION_PUBLIC_URL` and
  `ITERION_SMTP_FROM`: an operator fact, reviewed through the GitOps
  commit that deploys it.
- *B — the database.* A `legal` family of the platform settings ladder
  (ADR-090), a super-admin console, `GET/PUT /api/admin/settings/legal`.
  Rejected: a new durable state (and its cloud twin) for texts that
  change once a year; an audit trail to build; markdown edited live in a
  console with no reviewer; and a legal text that bypasses the GitOps
  review every other operator fact goes through. The ladder exists for
  OPERATIONAL settings that must change without a rollout; a legal notice
  is the opposite kind of fact.
- *C — the docs site.* `docs/legal/*.md` on the VitePress site, the studio
  linking out. Rejected: the docs site is English and another origin; the
  texts would commit one operator's publisher and DPO into an MIT
  repository served to every self-hosted operator; and the accessibility
  statement must describe THIS studio and be reachable from each of its
  pages — an outbound link does not satisfy the lint, nor the reader.
- *D — write it down as planned, publish nothing.* Rejected: the
  obligation stays open on an exposed service.

## Decision

**The legal content of a deployment is deployment configuration.** The
repository carries the SURFACE — the routes, the pages, the links, the
chart values, the templates — and never a text: no publisher, no DPO, no
hosting address, no conformance rate anywhere under version control
except as a `<placeholder>` in a template.

1. **One closed slug table** ([`pkg/legal.Slugs`](../../pkg/legal/legal.go)):
   `mentions-legales`, `confidentialite`, `cgu`, `accessibilite`. French
   and readable in the address bar on purpose. The loader, the route and
   the `legal` block of `/api/server/info` read this table; the studio
   links the server's list and names one slug itself (`accessibilite`,
   the statement the footer's accessibility mention leads to); the chart helper
   (`iterion.legal.documents`) is the one mirror, because a Helm template
   cannot import Go — a new document is a new row here and in that
   helper's list, and the chart's render test refuses a slug the helper
   does not know.
2. **Files, read once at startup.** `ITERION_LEGAL_DOCUMENTS_DIR` names a
   directory of `<slug>.md`; the chart renders `config.legal.documents.*`
   into a ConfigMap mounted there (or mounts
   `config.legal.existingConfigMap`). The server loads the directory
   before it starts and refuses to start on a directory it cannot read,
   a stray file (`cgu.markdown`, `mentions_legales.md`) or an empty
   document — half a surface is not published. A change to the documents
   the chart renders rolls the pod (`checksum/legal`), which is how the
   identity of what is served stays fixed between a review and a request.
   An `existingConfigMap` is outside the chart's sight: its change reaches
   the pods only through a rollout the operator triggers (a checksum of
   their own in `server.podAnnotations`, or `kubectl rollout restart`);
   until then a pod that restarts serves the new text beside pods that
   serve the old. Inline documents beside an `existingConfigMap` are
   refused at render — the inline texts would never be mounted. A cloud
   deployment that publishes no document starts, with a WARN at startup:
   the texts are the operator's to write, never the code's to refuse.
3. **A document that is not configured does not exist.** `GET
   /api/legal/{slug}` answers 404 for it and the SPA shows no link; the
   configured ones are listed in the `legal` block of `/api/server/info`,
   with the contact (`ITERION_LEGAL_CONTACT_EMAIL`) and the accessibility
   declaration (`ITERION_LEGAL_ACCESSIBILITY_STATUS` ∈ `non_compliant |
   partially_compliant | compliant`, `_DATE`, `_RATE`; the rate must match
   the status as the RGAA model reads it — 100 % compliant, 50 % to under
   100 % partially compliant and then required, under 50 % non compliant —
   and the contact is a bare address, published as a `mailto:`). The route
   is public by path (`isPublicPath`) and rate-limited per IP in a bucket
   of its own (`legal:<ip>`): the limiter refills a bucket at its caller's
   rate, so sharing the login family's bare-IP bucket would hand login
   attempts back on every anonymous read. It exists in every mode — the
   local server (`mode=local`) loads the same directory.
4. **One address, outside the studio base.** `/legal/<slug>` is a root
   side door (`ROOT_SIDE_DOORS`), never redirected to `/studio`, served to
   a visitor with or without a session, rendered `lang="fr"` from the
   markdown with the run console's `MarkdownText` (GFM on, raw HTML
   escaped). Links sit where the law expects them: the product home's
   footer with the « Accessibilité : <état> » mention, the sign-in card,
   the sidebar foot of every studio page, the foot of the two role shells
   that have no sidebar (`config_editor`, the restricted submitter), the
   public catalogue and the plan du site; a `mailto:` contact beside them.
   Inside the studio's router base every link names the root address
   (`legalRoute`, wouter's `~`), never `/studio/legal/<slug>`.
5. **The texts are the operator's, written with the DPO.** The repository
   ships French templates ([`docs/legal-templates/`](../legal-templates/))
   whose fields are the official models' (for the accessibility statement:
   status, date, rate, non-accessible content, establishment of the
   statement, feedback, recourse to the Défenseur des droits). The
   statement is published « non conforme » first — the honest default
   while the studio's RGAA audit is partial — and moves to « partiellement
   conforme » with a rate once a full audit backs it.

## Consequences

- Every operator of the chart — iterion.cloud, a ministry's own cluster,
  a laptop in `mode=local` with a yaml `legal:` block — publishes their own
  texts without a fork, and nobody inherits SocialGouv's publisher by
  running the image. The cost: the texts are a deploy, not a click; that
  is the review we want on a legal text.
- No new durable state, no cloud twin to prove, no audit trail to add:
  the GitOps history of the values IS the audit trail, and a replica set
  serves one text when the chart renders the documents (every change
  rolls every pod); with an `existingConfigMap` that consistency is the
  operator's rollout to trigger (decision 2).
- The public exposure checklist gains tickable items: pages published
  (`config.legal.*` set), accessibility statement published, DPO named in
  the sign-off. The RGAA audit's page-kind lint (contact, legal notice,
  accessibility statement) is satisfied once the pages are configured on
  the audited deployment.
- Pinned by tests at each site: the loader (order, stray and empty
  refusals, ConfigMap mount entries skipped), the config family (env,
  yaml, validation of status/date/rate/contact, rate against status, bare
  contact address), the startup (WARN in cloud mode with no document), the
  route (configured 200 with ETag revalidation — weak and listed tags
  included —, unconfigured 404, anonymous access through the auth
  middleware, 429 past the burst, a bucket that leaves the login budget
  alone), the server-info block (configured documents only, `[]` never
  `null`), the studio (links only for configured documents, root
  addresses from inside the studio base and the role shells, `lang="fr"`,
  raw HTML escaped, 404 copy, `/legal/` and `/legal/a/b` never blank), the
  chart (nothing rendered until configured, stray slug refused, existing
  ConfigMap honoured and refused beside inline documents, a text whose
  first line is indented, local StatefulSet mount).
- Out of scope, left to the operator: the texts themselves (DPO), the
  RGAA run that produces a rate (live, ~100 $), the `.ultra11yrc.json`
  sample growing the legal pages once a deployment has them, and the
  English versions a non-French operator may prefer (the documents are
  free text; nothing in the surface is French but the slugs, the titles
  and the chrome of the page).
