import { existsSync, readdirSync } from 'node:fs'
import { dirname, join, normalize } from 'node:path'
import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'
import type MarkdownIt from 'markdown-it'

const REPO = 'https://github.com/SocialGouv/iterion'
const BLOB = `${REPO}/blob/main`

// Extensions that are source artifacts, not site pages: even when referenced
// with an in-/docs relative path, link them to the file on GitHub.
const RAW_SOURCE = /\.(go|ebnf|ya?ml|sh|json|bot|botz|ts|tsx|mod|sum|toml|proto)$/i

// Docs are authored to be read on github.com: hundreds of links escape /docs
// (../pkg/*.go, ../README.md, ...) or point at raw in-repo source artifacts.
// Rewrite those to absolute GitHub blob URLs so they resolve on the site and
// are excluded from the dead-link check. Doc-to-doc .md links and images are
// left untouched for VitePress to resolve.
function githubLinks(md: MarkdownIt) {
  const defaultRender =
    md.renderer.rules.link_open ||
    ((tokens, idx, options, _env, self) => self.renderToken(tokens, idx, options))

  md.renderer.rules.link_open = (tokens, idx, options, env, self) => {
    const token = tokens[idx]
    const hrefIdx = token.attrIndex('href')
    if (hrefIdx >= 0) {
      const href = token.attrs![hrefIdx][1]
      const rewritten = rewriteHref(href, env?.relativePath)
      if (rewritten) {
        token.attrs![hrefIdx][1] = rewritten.href
        if (rewritten.external) {
          if (token.attrIndex('target') < 0) token.attrPush(['target', '_blank'])
          if (token.attrIndex('rel') < 0) token.attrPush(['rel', 'noreferrer'])
        }
      }
    }
    return defaultRender(tokens, idx, options, env, self)
  }
}

// The docs are authored for github.com and use angle-bracket placeholder
// notation everywhere (`<path>`, `<port>`, `<provider>`, `<path|git-url>`).
// markdown-it emits some of these as raw inline HTML, and VitePress's Vue
// compiler then chokes on them (invalid/duplicate attributes, unknown tags).
// Escape any raw HTML token whose tag is NOT a genuine HTML element, so
// placeholders render as literal text while real markup (<br/>, <details>,
// <img>, ...) is preserved.
const REAL_HTML_TAG =
  /^<\/?(a|abbr|b|blockquote|br|caption|code|col|colgroup|dd|details|div|dl|dt|em|figcaption|figure|h[1-6]|hr|i|img|kbd|li|mark|ol|p|picture|pre|q|s|samp|section|small|source|span|strong|sub|summary|sup|table|tbody|td|tfoot|th|thead|tr|u|ul|var|wbr)(\s|\/|>|$)/i

function escapeAngles(md: MarkdownIt) {
  const escape = (raw: string) => raw.replace(/</g, '&lt;').replace(/>/g, '&gt;')
  for (const rule of ['html_inline', 'html_block'] as const) {
    md.renderer.rules[rule] = (tokens, idx) => {
      const content = tokens[idx].content
      return REAL_HTML_TAG.test(content.trim()) ? content : escape(content)
    }
  }
}

// DSL template syntax (`{{input.field}}`, `{{vars.name}}`) fills these docs, in
// prose and in inline code. VitePress compiles page markdown as a Vue template,
// so a contiguous `{{ ... }}` is evaluated as an expression (breaking the build,
// or rendering empty). Neutralize the delimiters to HTML entities in prose text
// and inline-code tokens — the browser shows literal `{{ }}`. Fenced blocks are
// left alone: Shiki splits `{{` across separate spans, so Vue never sees a
// contiguous mustache there. This is scoped to markdown content and never
// touches the theme (unlike a global `vue.compilerOptions.delimiters`, which
// breaks VPHero/VPButton/nav).
function escapeBraces(md: MarkdownIt) {
  const neutralize = (html: string) =>
    html.replace(/\{\{/g, '&#123;&#123;').replace(/\}\}/g, '&#125;&#125;')
  const wrap = (rule: 'text' | 'code_inline', fallback: (t: any, i: number) => string) => {
    const prev = md.renderer.rules[rule] || fallback
    md.renderer.rules[rule] = (tokens, idx, options, env, self) =>
      neutralize(prev(tokens, idx, options, env, self))
  }
  wrap('text', (t, i) => md.utils.escapeHtml(t[i].content))
  wrap('code_inline', (t, i) => `<code>${md.utils.escapeHtml(t[i].content)}</code>`)
}

const DOCS_ROOT = join(__dirname, '..')

// Does an in-docs directory lack a browsable index (README.md / index.md)?
function dirHasNoIndex(resolved: string): boolean {
  const abs = join(DOCS_ROOT, resolved.replace(/^docs\/?/, ''))
  if (!existsSync(abs)) return false
  try {
    const entries = readdirSync(abs)
    return !entries.some((f) => /^(readme|index)\.md$/i.test(f))
  } catch {
    return false
  }
}

type Rewrite = { href: string; external: boolean }

function rewriteHref(href: string, relativePath: string | undefined): Rewrite | null {
  // Skip absolute URLs, anchors, and protocol-relative links.
  if (/^([a-z]+:)?\/\//i.test(href) || href.startsWith('#') || href.startsWith('mailto:')) {
    return null
  }
  const [path, hash = ''] = href.split('#')
  const suffix = hash ? '#' + hash : ''
  if (!path) return null

  // The doc's directory relative to the repo root (docs are under docs/).
  const docDir = relativePath ? join('docs', dirname(relativePath)) : 'docs'
  const resolved = normalize(join(docDir, path)).replace(/\\/g, '/')

  // The top-level docs index is excluded from the site (index.md is the home);
  // point in-docs links to it at the site home instead.
  if (resolved === 'docs/README.md') {
    // Internal link: VitePress prepends `base` itself, so omit it here.
    return { href: '/' + suffix, external: false }
  }

  const escapesDocs = !resolved.startsWith('docs/') && resolved !== 'docs'
  const isRawInDocs = resolved.startsWith('docs/') && RAW_SOURCE.test(path)
  // A directory link with no index page can't render as a site page — send it
  // to the GitHub tree so it still resolves.
  const isIndexlessDir = /\/$/.test(path) && dirHasNoIndex(resolved)

  if (escapesDocs || isRawInDocs) {
    return { href: `${BLOB}/${resolved}${suffix}`, external: true }
  }
  if (isIndexlessDir) {
    return { href: `${REPO}/tree/main/${resolved}${suffix}`, external: true }
  }
  return null
}

// Build a collapsed sidebar group listing every .md in a docs subdir.
// Returns null when the directory is absent or empty so the group is dropped.
function recordsGroup(text: string, dir: string) {
  const abs = join(__dirname, '..', dir)
  if (!existsSync(abs)) return null
  const items = readdirSync(abs)
    .filter((f) => f.endsWith('.md') && f.toLowerCase() !== 'readme.md')
    .sort()
    .map((f) => ({ text: f.replace(/\.md$/, ''), link: `/${dir}/${f.replace(/\.md$/, '')}` }))
  if (items.length === 0) return null
  return { text, collapsed: true, items }
}

const sidebar = [
  {
    text: 'Start here',
    items: [
      { text: 'Quickstart', link: '/quickstart' },
      { text: 'Current state', link: '/current-state' },
      { text: 'Changelog', link: '/changelog' },
      { text: 'Why Iterion', link: '/why-iterion' },
      { text: 'Philosophy', link: '/philosophy' },
      { text: 'Install', link: '/install' },
      { text: 'Examples', link: '/examples' },
      { text: 'CLI reference', link: '/cli-reference' },
      { text: 'Visual editor', link: '/visual-editor' },
      { text: 'Install into AI agents (skill)', link: '/skill' },
      { text: 'MCP server (drive from agents)', link: '/mcp-server' },
      { text: 'Why not prompt orchestration', link: '/why-not-prompt-orchestration' },
      { text: 'Asymptote bench', link: '/asymptote-bench' },
      { text: 'The ratchet', link: '/improvement-ratchet' },
      { text: 'Thinking metrics', link: '/thinking-metrics' },
    ],
  },
  {
    text: 'Author .bot workflows',
    items: [
      { text: 'DSL guide', link: '/dsl' },
      { text: 'DSL grammar (readable)', link: '/references/dsl-grammar' },
      { text: 'V1 grammar scope', link: '/grammar/V1_SCOPE' },
      { text: 'Diagnostics catalogue', link: '/references/diagnostics' },
      { text: 'Routers', link: '/routers' },
      { text: 'Groups, iteration & subbots', link: '/groups-iteration-subbots' },
      { text: 'Human in the loop', link: '/human-in-the-loop' },
      { text: 'Cursors', link: '/cursors' },
      { text: 'Supervisors', link: '/supervisors' },
      { text: 'Totality & Turing-completeness', link: '/dsl-totality-and-tc' },
      { text: 'Recipes', link: '/recipes' },
      { text: 'Attachments', link: '/attachments' },
      { text: 'Bundles', link: '/bundles' },
      { text: 'Import', link: '/import' },
      { text: 'Backends', link: '/backends' },
      { text: 'Delegation', link: '/delegation' },
      { text: 'Permissions', link: '/permissions' },
      { text: 'Skills library', link: '/skills-library' },
      { text: 'Plugins', link: '/plugins' },
      { text: 'Memory & knowledge', link: '/memory-and-knowledge' },
      { text: 'Web search', link: '/web-search' },
      { text: 'Ultracode', link: '/ultracode' },
      { text: 'Secrets', link: '/secrets' },
      { text: 'Secrets reference', link: '/secrets-reference' },
      { text: 'Privacy filter', link: '/privacy_filter' },
      { text: 'Authoring pitfalls', link: '/workflow_authoring_pitfalls' },
      { text: 'Patterns', link: '/references/patterns' },
      { text: 'Productive session patterns', link: '/references/productive-session-patterns' },
      { text: 'References bootstrap', link: '/references-bootstrap' },
    ],
  },
  {
    text: 'Run and operate locally',
    items: [
      { text: 'Bot invocations', link: '/bot-invocations' },
      { text: 'Resume', link: '/resume' },
      { text: 'Merge policy', link: '/merge-policy' },
      { text: 'Review & merge gate', link: '/review-merge-gate' },
      { text: 'Sandbox', link: '/sandbox' },
      { text: 'Scheduling', link: '/scheduling' },
      { text: 'Dispatcher', link: '/dispatcher' },
      { text: 'Native tracker', link: '/native-tracker' },
      { text: 'Session board', link: '/session-board' },
      { text: 'Repo scope', link: '/repo-scope' },
      { text: 'Settings precedence', link: '/settings-precedence' },
      { text: 'Environment variables', link: '/environment-variables' },
      { text: 'Config share', link: '/config-share' },
      { text: 'Browser pane', link: '/browser-pane' },
      { text: 'Post-mortem shell', link: '/post-mortem-shell' },
      { text: 'Persisted formats', link: '/persisted-formats' },
      { text: 'Observability', link: '/observability/README' },
    ],
  },
  {
    text: 'Bots & security automation',
    items: [
      { text: 'Examples', link: '/examples' },
      { text: 'Security bots', link: '/security-bots' },
      { text: 'Security bots (distributed)', link: '/security-bots-distributed' },
      { text: 'Security patcher', link: '/security-patcher' },
    ],
  },
  {
    text: 'Cloud / agent control plane',
    items: [
      { text: 'Cloud overview', link: '/cloud-overview' },
      { text: 'Cloud components', link: '/cloud' },
      { text: 'Cloud user guide', link: '/cloud-user' },
      { text: 'Forge integrations', link: '/forge-integrations' },
      { text: 'Forge permissions', link: '/forge-permissions' },
      { text: 'Forge conversations', link: '/forge-conversations' },
      { text: 'Webhooks', link: '/webhooks' },
      { text: 'Outbound callbacks', link: '/outbound-callbacks' },
      { text: 'BYOK', link: '/byok' },
      { text: 'OAuth forfait', link: '/oauth-forfait' },
      { text: 'Quotas & limits', link: '/quotas-and-limits' },
      { text: 'Cloud CLI (remote)', link: '/cloud-cli' },
      { text: 'Cloud REST API', link: '/cloud-rest-api' },
      { text: 'Cloud LLM credentials', link: '/cloud-llm-credentials' },
      { text: 'Cloud deployment', link: '/cloud-deployment' },
      { text: 'Queue schema rollout runbook', link: '/cloud-queue-schema-rollout' },
      { text: 'Cloud architecture', link: '/cloud-architecture' },
      { text: 'Cloud admin guide', link: '/cloud-admin-guide' },
      { text: 'Cloud admin bootstrap', link: '/cloud-admin' },
      { text: 'Cloud backup', link: '/cloud-backup' },
      { text: 'Probes & graceful shutdown', link: '/probes-and-graceful-shutdown' },
      { text: 'Cloud troubleshooting', link: '/cloud-troubleshooting' },
      { text: 'Public exposure checklist', link: '/cloud-public-exposure-checklist' },
      { text: 'CI performance (BuildKit operator)', link: '/ci-performance-buildkit-operator' },
    ],
  },
  {
    text: 'Desktop',
    items: [
      { text: 'Desktop app', link: '/desktop' },
      { text: 'Desktop architecture', link: '/desktop-architecture' },
      { text: 'Desktop build', link: '/desktop-build' },
      { text: 'Desktop distribution', link: '/desktop-distribution' },
      { text: 'Desktop QA', link: '/desktop-qa' },
      { text: 'Desktop QA checklist', link: '/desktop-qa-checklist' },
      { text: 'Desktop release checklist', link: '/desktop-release-checklist' },
    ],
  },
  {
    text: 'Architecture & contribution',
    items: [
      { text: 'Architecture', link: '/architecture' },
      { text: 'Development', link: '/development' },
      { text: 'E2E coverage', link: '/e2e_coverage' },
      { text: 'Live E2E coverage', link: '/live-e2e-coverage' },
    ],
  },
  {
    text: 'Point-in-time records',
    collapsed: true,
    items: [
      recordsGroup('ADRs', 'adr'),
      recordsGroup('Changelog archives', 'changelog'),
      recordsGroup('Bot-run bilans', 'bot-runs'),
      recordsGroup('Plans', 'plans'),
      recordsGroup('Reviews', 'reviews'),
      recordsGroup('Security audits', 'security'),
    ].filter((g): g is NonNullable<typeof g> => g !== null),
  },
]

export default withMermaid(
  defineConfig({
    title: 'Iterion',
    description: 'The control plane for AI agents.',
    base: '/iterion/',
    lang: 'en-US',
    cleanUrls: true,
    lastUpdated: true,
    // The top-level README.md stays the github.com index; the site home is
    // index.md. Nested READMEs build to `<dir>/README.html` (VitePress does not
    // treat README as a directory index), so link them as `/<dir>/README`, never
    // the bare `/<dir>/` directory (which would 404).
    srcExclude: ['README.md'],
    // localhost:* appears as illustrative example URLs in operator docs.
    ignoreDeadLinks: [/^https?:\/\/localhost/],
    head: [
      ['link', { rel: 'icon', href: '/iterion/favicon.ico' }],
      ['meta', { property: 'og:type', content: 'website' }],
      ['meta', { property: 'og:site_name', content: 'Iterion' }],
      ['meta', { property: 'og:title', content: 'Iterion — the control plane for AI agents' }],
      [
        'meta',
        {
          property: 'og:description',
          content:
            'Define agent workflows as readable .bot files and operate every run — locally, in CI, or across a multi-tenant cloud.',
        },
      ],
      ['meta', { property: 'og:image', content: 'https://socialgouv.github.io/iterion/og.png' }],
      ['meta', { property: 'og:url', content: 'https://socialgouv.github.io/iterion/' }],
      ['meta', { name: 'twitter:card', content: 'summary_large_image' }],
      ['meta', { name: 'twitter:title', content: 'Iterion — the control plane for AI agents' }],
      [
        'meta',
        {
          name: 'twitter:description',
          content:
            'Define agent workflows as readable .bot files and operate every run — locally, in CI, or across a multi-tenant cloud.',
        },
      ],
      ['meta', { name: 'twitter:image', content: 'https://socialgouv.github.io/iterion/og.png' }],
    ],
    markdown: {
      // The .bot DSL uses ```iter fences (YAML-like, indentation-based) — alias
      // to the bundled yaml grammar. (```ebnf isn't bundled either but has no
      // close bundled match; it falls back to plain text on its own.)
      languageAlias: { iter: 'yaml' },
      config: (md) => md.use(escapeBraces).use(escapeAngles).use(githubLinks),
    },
    themeConfig: {
      logo: '/iterion-logo.png',
      search: { provider: 'local' },
      nav: [
        { text: 'Why Iterion?', link: '/why-iterion' },
        {
          text: 'Get started',
          items: [
            { text: 'Quickstart (AI devs)', link: '/quickstart' },
            { text: 'Install & all modes', link: '/install' },
          ],
        },
        { text: 'DSL', link: '/dsl' },
        { text: 'Bots', link: '/examples' },
        { text: 'Cloud', link: '/cloud-overview' },
      ],
      sidebar,
      editLink: {
        pattern: `${REPO}/edit/main/docs/:path`,
        text: 'Edit this page on GitHub',
      },
      socialLinks: [{ icon: 'github', link: REPO }],
    },
  }),
)
