# Legal page templates

The four markdown templates an operator fills in — with their DPO and
legal reviewer — and ships as deployment configuration
([ADR-160](../adr/160-legal-content-is-deployment-configuration.md)): the
repository carries the surface and these skeletons, never a text. A
`<placeholder>` is a fact only the operator holds; an unfilled placeholder
must not reach a deployment.

| File | Slug (`/legal/<slug>`) | Obligation |
|---|---|---|
| [mentions-legales.md](mentions-legales.md) | `mentions-legales` | LCEN art. 6-III: publisher, publication director, hosting provider, contact |
| [confidentialite.md](confidentialite.md) | `confidentialite` | RGPD art. 13: controller, purposes, legal bases, data, recipients and processors (AI providers, transfers outside the EU), retention, rights, DPO, cookies |
| [cgu.md](cgu.md) | `cgu` | Terms of use (skeleton) |
| [accessibilite.md](accessibilite.md) | `accessibilite` | Accessibility statement (loi 2005-102 art. 47, décret 2019-768): conformance status, date, rate, non-accessible content, establishment of the statement, feedback, recourse |

How they reach a deployment, and how the contact and the accessibility
status are set: [cloud-admin.md § Legal pages](../cloud-admin.md#legal-pages-configlegal).

Writing rules the page relies on:

- The page sets the `h1` (the document's title); start the text's own
  sections at `#` — they render one level below the title.
- GitHub-flavoured markdown is rendered; raw HTML is escaped, never
  interpreted.
- A reference to another legal page is written as its address in code
  (`/legal/confidentialite`) in these templates, because the docs site
  publishes them and checks every link against its own pages; in your copy,
  make it a link: `[politique de confidentialité](/legal/confidentialite)`.
- The texts are what the end user reads: French for a French public
  service. Nothing in the surface requires French — an operator elsewhere
  writes in their own language.
- Publish the accessibility statement as « non conforme » until a full
  RGAA audit of the studio gives a rate; then the status the rate earns,
  with the rate and the audit date — the server refuses a rate that
  contradicts its status (100 % = totalement conforme, 50 % to under
  100 % = partiellement conforme, under 50 % = non conforme).
