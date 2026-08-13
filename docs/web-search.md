# Web search & fetch

How agents discover and read the web in iterion, as a **capability ladder**
you climb only as far as your needs (and your sovereignty constraints)
require. Search stays **one tool** (`web_search`) whose backend is resolved
from the environment; scraping is either the built-in `web_fetch` or, at the
top tier, Firecrawl over MCP.

| Tier | Infra to run | Search backend | Scrape / crawl |
|------|--------------|----------------|----------------|
| 0 | nothing | DuckDuckGo Lite (scrape) | `web_fetch` (HTTP GET + strip HTML) |
| 1 | a Brave API key | Brave Search API | `web_fetch` |
| 2 | **one SearXNG container** | **SearXNG** (sovereign) | `web_fetch` |
| 3 | Firecrawl + SearXNG | Firecrawl `search` | Firecrawl (JS render, readability, crawl, extract) |

## The two built-in tools (claw backend)

The `claw` backend ships two client-side web tools from `claw-code-go`:

- **`web_fetch`** — fetch a known URL, strip HTML to text (15s, 50 KB cap).
  Registered **by default**. No search/discovery: you must already have the URL.
- **`web_search`** — query the web, return `title / url / snippet` rows. Its
  backend is resolved at execute time by env precedence:

  ```
  SEARXNG_URL (or SEARXNG_ENDPOINT)  →  a self-hosted SearXNG instance
  else BRAVE_API_KEY                 →  the Brave Search API
  else                               →  DuckDuckGo Lite (HTML scrape)
  ```

  A configured **sovereign** instance wins over any external service, so
  queries never leak to Brave/Google once `SEARXNG_URL` is set.

### Enabling `web_search`

`web_search` is gated (its zero-config DDG fallback is brittle and leaks
queries). The gate is `ITERION_WEB_SEARCH`, resolved by
[`tool.ResolveWebSearchEnabled`](../pkg/backend/tool/web_search_enable.go):

| `ITERION_WEB_SEARCH` | Effect |
|----------------------|--------|
| `on` / `true` / `1`  | always register (DuckDuckGo scrape included) |
| `off` / `false` / `0`| never register |
| unset / `auto` (default) | register **only** when a search backend is configured (`SEARXNG_URL`, `SEARXNG_ENDPOINT`, or `BRAVE_API_KEY`) |

So in the default `auto` mode, standing up a SearXNG container (or setting a
Brave key) is all it takes to give claw agents real web search; the
low-quality DDG scrape is reachable only by explicitly forcing `on`.

A node still has to allow the tool — list `web_search` in the node's `tools:`
(or leave `tools:` unset on claw to inherit the full set).

### SearXNG setup (tier 2)

Run one SearXNG container and **enable its JSON output format** (required —
`web_search` requests `format=json`, and an instance without it returns
HTTP 403, surfaced as an explicit error):

```yaml
# searxng settings.yml
search:
  formats:
    - html
    - json
```

Then point iterion at it:

```sh
export SEARXNG_URL=http://localhost:8080   # activates SearXNG + auto-enables web_search
```

## Tier 3 — Firecrawl over MCP (`firecrawl` builtin plugin)

For JS-rendered pages, readability extraction, crawling, and structured
extract, use **Firecrawl**, which iterion wires as an MCP server through the
built-in `firecrawl` plugin (disabled by default):

```sh
iterion plugin enable firecrawl
# self-hosted Firecrawl? set its URL (leave empty for Firecrawl cloud):
iterion plugin config firecrawl api_url=http://localhost:3002
```

The plugin contributes an `mcp.firecrawl.*` server to the workflow MCP
catalog. A claw node uses it like any MCP tool:

```iter
agent researcher:
  backend: claw
  tools: [mcp.firecrawl.search, mcp.firecrawl.scrape, web_fetch]
  mcp:
    servers: [firecrawl]
```

### Pairing Firecrawl with SearXNG (sovereign tier 3)

Firecrawl's own `search` endpoint delegates to a configured search provider.
To keep the whole stack sovereign, point **Firecrawl** (not the MCP client)
at your SearXNG instance via its deployment env:

```sh
# in Firecrawl's docker-compose / env, NOT in iterion:
SEARXNG_ENDPOINT=http://searxng:8080
```

iterion just connects to Firecrawl; Firecrawl uses SearXNG internally. So a
single SearXNG instance can serve both claw's `web_search` (tier 2) and
Firecrawl's `search` (tier 3).

## Backend note: claude_code, claw, and pi

- **claude_code** has its own native `WebSearch` / `WebFetch` (Claude Code
  CLI). A `claude_code` node that leaves `tools:` unset inherits them for
  free. If it restricts `tools:`, list `WebSearch` / `WebFetch` explicitly.
  Pi has no native web-search tool; use an MCP server there.
- **External MCP servers (Firecrawl, custom SearXNG MCP) reach all three
  MCP-capable backends.** `claw` resolves them into tools in-process;
  `claude_code` forwards the active `mcp_server` / plugin configs via
  `--mcp-config` ([wireUserMCP](../pkg/backend/delegate/claude_code_hooks.go));
  pi RPC hands the same resolved configs to its embedded extension, whose MCP
  client supports streamable HTTP, legacy SSE, and stdio. Pi print mode, Kimi,
  Grok, and Codex do not consume this catalog.
- For a sandboxed stdio server, `command:` resolves inside the container for
  Claude Code and pi, so a host-only path fails there. Prefer an `http`/`sse`
  server for sandboxed runs; a self-hosted Firecrawl MCP over HTTP is reachable
  from inside the container.

`web_fetch` (claw) and the tier resolution above are unrelated to Anthropic's
server-side `web_search_*` / `web_fetch_*` tool types — claw does not pass
those through; all web work is client-side.
