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

## Polled triggers

A trigger with `delivery: poll` is read by a poller rather than pushed by a
webhook: one **read** operation of the same package, called on an interval
([ADR-168](adr/168-an-event-source-is-a-connector-package.md) §7). The
example is a fictional `probe` vendor, loaded as written by
`TestAPolledTriggerLoadsFromItsYAML`:

```yaml
- id: probe.issue.updated
  delivery: poll
  poll:
    operation: probe.issue.search      # a read operation of this package
    params:                            # expressions over query and cursor
      q: 'query.q + " AND updated >= " + if(cursor.value, cursor.value, "-1d")'
    query: [ { id: q, required: true } ]   # what the operator types
    item_id: 'item.key'
    item_version: 'item.fields.updated'
    cursor: { from: 'item.fields.updated', kind: timestamp, overlap: 1m }
    interval: { default: 5m, min: 1m }
  outputs:
    - { name: subject, from: 'payload.fields.summary' }
  sample: samples/issue-updated.json   # {"response": <one decoded answer>}
```

- **A polled trigger declares no `event`.** Its poller names it, and no
  delivery reaches it: both lanes that route a delivery by its event name go
  through one lookup that skips polled triggers, and an event that arrives
  through an ingress is never matched to a polled trigger (nor a poller's to
  a webhook one) — a package that turns a trigger from one to the other
  leaves no stored event that can launch through it.
- **The walk is the executor's.** A paginated operation is walked by
  `exec.CallPaged`, so its items are its pagination's `items_field`
  (`poll.items` is refused there, required otherwise), and the parameters the
  walk owns — the page and the cursor parameter — are refused in
  `poll.params`: building one would read a single hand-picked page, never
  complete, and never move the checkpoint. A required parameter the walk does
  not send on the first page cannot be required.
- **Every tick must be askable.** At load, each param is evaluated as the
  first tick sees it (no checkpoint, optional query fields unset) and, when
  the poll declares a cursor, as a later tick does: an expression that fails,
  or a required parameter that builds nothing, is refused. A params block that
  reads `cursor.value` needs a `cursor:` block, and a `cursor:` block some
  param must read. Query fields and the cursor are read by name
  (`query.q`), never by index.
- **Identity.** `item_id` (required) and `item_version` read `item` — and
  `response` too on an unpaginated operation; a paginated walk hands back its
  last page as the response. Without `item_version` an item's revision is the
  digest of the item itself. A number is read in its decimal digits; one at
  or past 2^53, where decoded JSON can no longer tell two ids apart, is
  refused. An item seen at one (id, version) is delivered once.
- **The event is the item.** `outputs`, `filters` and `ignore` read the item
  as `payload` — one vocabulary for pushed and polled events — and no
  `headers`, which an item has none of.
- **Cursors and instants.** A `timestamp` cursor reads RFC 3339, Jira's
  offset without a colon, and the RFC 822 dates of RSS — a numeric zone, or
  one of the names RFC 822 defines (UT, GMT, EST, EDT, CST, CDT, MST, MDT,
  PST, PDT) at its own offset, never the process's; any other zone name is
  refused. An `opaque` cursor is a string or number, handed back as is.
- **The interval** is counted in seconds: `min` at least `1s` and at most
  `default`.
- **Feeds.** An operation with `response_format: feed` reads a successful
  answer as RSS 2.0 or Atom 1.0 into one shape: `{title, link, updated,
  items: [{id, version, title, link, summary, content, published, updated,
  author, categories}]}`. An item's `id` falls back from its guid (Atom `id`)
  to its link to the digest of its content; its `version` is the digest of
  every field a reader sees, so an edit is a new revision and whitespace or
  the order of categories is not. Instants that parse are rewritten as RFC
  3339 in UTC; one that does not is kept as written. RSS fields are matched
  in RSS's own (empty) namespace, so an `atom:link`, an `itunes:author` or a
  `media:description` replaces nothing; an Atom entry with no author has the
  feed's, and an xhtml text keeps its markup without the wrapping `div`. The
  charset is the `Content-Type` header's when it names one, else the
  document's declaration — WHATWG's labels for UTF-8 and windows-1252 (which
  Latin-1 and US-ASCII labels map to) — and any other is refused by name. A
  feed is bounded in items (`ITERION_CONNECTOR_FEED_MAX_ITEMS`, 10000) and in
  nesting (`ITERION_CONNECTOR_FEED_MAX_DEPTH`, 256), refused past either and
  never cut; `0` removes a bound. A feed operation cannot carry a JSON
  `response_schema_ref`, and an error answer is never decoded as a feed.
- **The sample** is one recorded answer, `{"response": …}`: every item in it
  must be an object with an identity and a cursor the declared kind reads,
  each output must read its type, and each output must read something on at
  least one item — or the package does not load.
- **One event, one webhook trigger.** Two webhook triggers declaring the same
  event are refused: a delivery reaches one trigger, so the second would be
  offered and never fire. A trigger that narrows the event does it with its
  filters.

The format, the decoder and the routing are in place; pollers themselves ship
with L4-2 (#3274). Until then the catalogue offers only the triggers a
delivery can reach — webhook triggers of a package that declares its
ingress — an ingress cannot be bound to a package without one, and a
subscription to a polled trigger, or to a trigger of a package with no
ingress, is refused at save, naming why, rather than accepted to never fire.

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
