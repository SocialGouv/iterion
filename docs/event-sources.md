# Event sources

An event source is anything that launches a bot: a connector package's
inbound webhook, or one of the built-in families (a schedule, a board
card, a run outcome, an event another bot emits). The studio's
**Sources** view is the catalog of what this deployment can plug, and
the plug wizard wires a source to a bot in one pass.

## The catalog

`GET /api/v1/sources` (and `/api/teams/{id}/sources` for a team's view)
is a read model: it stores nothing. One card per connector package that
declares triggers — across the tiers that serve the tenant (team,
platform, baked) — with:

- the presentation fields (`category`, `brand_color`, the icon),
- the maturity and the tier that served the package,
- the delivery mode (`push` today; polling arrives with ADR-168 §7),
- the package's auth schemes and its declared triggers, with the values
  a delivery carries (`outputs`) and the narrowings an operator may
  pick (`filters`),
- the ingress contract summary: where the credential travels, who mints
  the secret, whether deliveries name an installation,
- how many connections and subscriptions this tenant already holds for
  it.

A package that declares no trigger is an outbound tool, not a source,
and is not listed. Beside the connectors, the built-in families list as
descriptors that route to the dialogs they already have (the schedules
tab, the board selector, run completion, `/api/v1/triggers/emit`).

## Plugging a source

The wizard (`/sources/plug?source=…`, or a bot page's connector row) is
URL-driven — every choice lives in the URL, so a reload or a shared
link reopens the same step:

1. **Account** — pick one of the tenant's connections for this
   connector. Connections are created in Integrations.
2. **Event** — pick the trigger and, where the package declares
   filters, the narrowing.
3. **Bot** — pick the bot the source launches.
4. **Mapping** — name the bot variable each declared output lands in.
   The preview reads the package's recorded sample through the same
   extraction, filters and judgment the lane will run (`POST
   /api/v1/triggers/preview`) and writes nothing.
5. **Enable** — the connector webhook config bound to the connection is
   reused or minted (the secret is shown once), and the subscription is
   created; the save judges the binding against the connection, the
   package's trigger and the bot's declared vars.

The vendor side is the last manual step: declare the delivery URL and
the secret on the vendor's webhook settings. When the package ships a
`setup.md`, the review step shows it verbatim — the package's own
instructions for that vendor's settings screens. Packages that declare how
to provision their hook automate this in a later wave (ADR-168 §6).

## Icons

A package may ship `icon:` (a file in the package, `icon.svg` by
convention), read once at load: elements and attributes that carry
script or stylesheet content, event handlers, comments and every
reference that leaves the document are dropped, and the route serves
the sanitized bytes — never the file — as `image/svg+xml` under
`Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline';
sandbox`, with the digest as the ETag. A package with no icon gets a
monogram on its `brand_color`, in the foreground its contrast picks.
`ITERION_CONNECTOR_ICON_MAX_BYTES` caps the file (64 KiB by default; 0
removes the cap; a value that is not a byte count refuses the load).
