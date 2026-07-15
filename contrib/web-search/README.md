# Sovereign web-search stack for iterion

A reference `docker compose` for the two self-hosted tiers of iterion's web
search (see [../../docs/web-search.md](../../docs/web-search.md)):

- **Tier 2 — SearXNG**: sovereign metasearch. Powers claw's `web_search`
  (and, forwarded, claude_code). One light container. No query leaves your
  network.
- **Tier 3 — Firecrawl**: JS-rendered scrape / crawl / extract, with its own
  `search` delegated to the same SearXNG. Heavier (redis + rabbitmq +
  postgres + playwright).

## Quick start (tier 2 — SearXNG only)

```sh
cd contrib/web-search
cp .env.example .env
sed -i "s/please-change-this-local-secret/$(openssl rand -hex 32)/" .env

docker compose up -d searxng

# Verify the JSON API (must be 200 with a results array):
curl -s "http://localhost:8080/search?q=test&format=json" | head -c 200
```

Point iterion at it. Under the default `ITERION_WEB_SEARCH=auto`, setting
`SEARXNG_URL` both selects the SearXNG backend AND auto-enables `web_search`:

```sh
export SEARXNG_URL=http://localhost:8080
```

Then run a claw node with `web_search` in its `tools:` — e.g. the example
[../../examples/web-search/research.bot](../../examples/web-search/research.bot):

```sh
iterion run examples/web-search/research.bot --store-dir "$PWD/.iterion"
```

Precedence is `SEARXNG_URL` → `BRAVE_API_KEY` → DuckDuckGo, so a configured
SearXNG always wins.

## Tier 3 — Firecrawl (opt-in)

Firecrawl's `search` delegates to the SearXNG above (`SEARXNG_ENDPOINT`), so
one instance serves both tiers.

```sh
docker compose --profile firecrawl up -d      # pulls redis/rabbitmq/postgres/playwright/firecrawl

# Verify (all five services + firecrawl reachable):
curl -s http://localhost:3002/                          # → {"message":"Firecrawl API", ...}
curl -s -X POST http://localhost:3002/v1/search \
  -H 'Content-Type: application/json' \
  -d '{"query":"iterion","limit":3}'                    # routed through SearXNG
curl -s -X POST http://localhost:3002/v1/scrape \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","formats":["markdown"]}'

# Enable the iterion plugin (disabled by default) and point it at the local API:
iterion plugin enable firecrawl
iterion plugin config firecrawl api_url=http://localhost:3002
```

A claw node then uses `mcp.firecrawl.search` / `mcp.firecrawl.scrape` with
`mcp: { servers: [firecrawl] }`. On claude_code the same servers are
forwarded via `--mcp-config` (native WebSearch/WebFetch stay on too).

The images are pre-built (`ghcr.io/firecrawl/*`); no local build. To pin a
version, replace `:latest` with a digest.

## SearXNG settings

[searxng/settings.yml](searxng/settings.yml) inherits SearXNG's defaults and
overrides only what iterion needs. The **load-bearing** line is:

```yaml
search:
  formats:
    - html
    - json      # REQUIRED — without it, /search?format=json → HTTP 403
```

`server.limiter: false` disables bot-detection for a private instance; keep
it **behind your own network boundary** (don't expose an unlimited instance
publicly).

## From local to infra / prod

The same two services move to your cluster unchanged; only the wiring differs:

1. **Deploy** SearXNG (and optionally Firecrawl) on the infra — a Deployment
   + Service each, mounting the same `settings.yml` (JSON format enabled),
   `SEARXNG_SECRET` from a Secret. Keep them **cluster-internal** (no public
   ingress) — they are unauthenticated by design.
2. **Wire prod iterion**: set on the iterion runtime/runner environment
   - `SEARXNG_URL=http://searxng.<namespace>.svc:8080` (tier 2, auto-enables
     `web_search`), and
   - for tier 3, `iterion plugin enable firecrawl` +
     `firecrawl.api_url=http://firecrawl.<namespace>.svc:3002`, with
     Firecrawl's own `SEARXNG_ENDPOINT` pointing at the in-cluster SearXNG.
3. **Sandboxed runs**: prefer the http/sse transport for the Firecrawl MCP so
   it's reachable from inside the container (a stdio server's host `command:`
   is not) — see [../../docs/web-search.md](../../docs/web-search.md).

Nothing here is iterion-specific: it's a plain SearXNG + Firecrawl stack that
any consumer of `SEARXNG_URL` / the Firecrawl API can share.
