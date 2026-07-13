# ADR-072 — Web search as a sovereign capability ladder (SearXNG → Firecrawl)

Status: **accepted** (2026-07-13). Ships with the implementation
(claw SearXNG backend + `ITERION_WEB_SEARCH` resolver + `firecrawl`
builtin plugin + [docs/web-search.md](../web-search.md)).

## Context

Before this change, web search in iterion was incomplete and, for the
default backend, effectively dormant:

- **claw** (`claw-code-go`) shipped two client-side tools: `web_fetch`
  (HTTP GET + strip HTML, registered by default) and `web_search`
  (`BRAVE_API_KEY` → Brave, else a DuckDuckGo Lite scrape).
- **iterion** registered `web_search` only behind `ClawDefaults.IncludeWebSearch`,
  and **no production code ever set that flag** — so claw agents could
  `web_fetch` a known URL but never *discover* one.
- **claude_code** got web search for free (native `WebSearch`/`WebFetch`)
  when a node left `tools:` unset.
- The **MCP** layer (`mcp_server` DSL, plugin `mcp_servers`) already
  existed and was first-class, but resolved into tools **only for the
  claw backend**.

Two forces shaped the decision. First, **sovereignty**: the primary
operator is a French public-sector org, so a default that leaks every
search query to Brave/Google (or scrapes DuckDuckGo) is undesirable; a
self-hosted search path must be a first-class, preferred option.
Second, **not multiplying the agent's surface**: search should stay one
tool the agent already knows (`web_search`), not a new per-provider tool
per deployment.

## Options considered

1. **Provider-per-tool** — add `brave_search`, `searxng_search`,
   `firecrawl_search` as distinct tools. Rejected: forces the agent (and
   every bot author) to know which tool a given deployment wired, and
   couples bots to infra.
2. **Anthropic server-side web tools** — pass through
   `web_search_20250305`. Rejected: claw's `api.Tool` has no `type`
   field, it is Anthropic-only, and it defeats sovereignty (search runs
   on Anthropic's servers).
3. **Firecrawl-only** — require the full Firecrawl stack for any search.
   Rejected: too heavy for the common case (a single SearXNG container,
   or just a Brave key, is enough for search without JS rendering).
4. **A resolved capability ladder** (chosen) — search stays one tool
   whose backend is resolved from the environment; scraping is the
   built-in `web_fetch` until the top tier, where Firecrawl (over the
   existing MCP seam) adds JS render / crawl / extract.

## Decision

Adopt a four-tier ladder, each tier reusing an existing seam:

| Tier | Infra | Search | Scrape/crawl |
|------|-------|--------|--------------|
| 0 | none | DuckDuckGo Lite | `web_fetch` |
| 1 | Brave key | Brave API | `web_fetch` |
| 2 | one SearXNG container | **SearXNG** | `web_fetch` |
| 3 | Firecrawl + SearXNG | Firecrawl `search` | Firecrawl (JS, crawl, extract) |

Load-bearing decisions:

- **SearXNG lives in claw's `web_search`**, alongside Brave/DDG, with
  precedence `SEARXNG_URL`/`SEARXNG_ENDPOINT` → `BRAVE_API_KEY` → DDG. A
  configured sovereign instance wins over any external service. It stays
  one tool; the agent learns nothing new. (Upstream, so every claw
  consumer benefits; iterion vendors it.)
- **`web_search` enablement is resolved, not hardcoded**:
  `ITERION_WEB_SEARCH=on|off|auto` (default `auto`) via
  `tool.ResolveWebSearchEnabled`. `auto` registers the tool only when a
  search backend is configured (SearXNG/Brave), keeping the brittle,
  query-leaking DDG scrape **opt-in** (reachable only via `on`). Wired at
  the single `ClawDefaults` construction site, shared by the local and
  cloud runner paths.
- **Firecrawl is a builtin plugin** (`mcp_servers` contribution,
  disabled by default), reusing the plugin/MCP machinery — zero engine
  code. SearXNG pairs with it via Firecrawl's own `SEARXNG_ENDPOINT`, so
  one SearXNG instance serves both tier 2 and tier 3.

## Consequences

- Standing up a SearXNG container (JSON format enabled) is all it takes
  to give claw agents sovereign search under the default `auto` mode.
- **Firecrawl/MCP web tools are claw-only.** iterion does not forward
  user-declared MCP servers to the claude_code/codex CLIs (only the
  internal `ask_user` + board servers), so on claude_code the search path
  is its native `WebSearch`/`WebFetch`. Forwarding user MCP servers to
  the claude CLI via `--mcp-config` is a deferred follow-on that would
  close this gap and make the ladder backend-symmetric.
- A SearXNG instance must have the `json` output format enabled; absence
  surfaces as an explicit HTTP-403 error, not a silent empty result
  (per the erreurs-explicites principle).
- No dependency on Anthropic server-side web tools; all web work stays
  client-side (claw) or in the operator's own Firecrawl (tier 3).
