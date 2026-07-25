---
name: lang-js
description: |
  JS/TS scanner reference for sec-audit-source. Covers semgrep
  with the javascript/typescript/nodejs rule packs, plus
  framework-specific threat hints for Express, Fastify, Next.js
  and NestJS. Loaded when tech.langs ∋ js or ts.
---

# lang-js — JavaScript / TypeScript scanners

Activated by the `run_js_scanners` branch when `detect_tech` reports
`js` or `ts` in `tech.langs`. Project layout signals: `package.json`
at workspace root, `tsconfig.json`, or any `*.ts` / `*.tsx` /
`*.mjs` files.

## Primary scanner — semgrep

```bash
semgrep \
  --config=p/javascript \
  --config=p/typescript \
  --config=p/nodejsscan \
  --config=p/owasp-top-ten \
  --json \
  --output={{vars.scan_dir}}/js.json \
  --error \
  --metrics=off \
  --quiet \
  --exclude='node_modules' \
  --exclude='.iterion' \
  --exclude='.sec-audit' \
  --exclude='dist' \
  --exclude='build' \
  --exclude='.next' \
  --exclude='*.min.js' \
  {{vars.workspace_dir}}
```

The four rule packs together cover ~600 rules. `nodejsscan` adds
server-specific patterns the generic JS pack misses (e.g.
`child_process.exec` with template strings).

## Framework-specific threat hints

The `triage` agent uses `detect_tech.frameworks` to weight findings
and to bump severity. Patterns to weight specifically:

### Express
- `app.use(express.urlencoded({ extended: true }))` enables
  prototype pollution surface — flag together with any `merge()`
  or `extend()` call downstream.
- Routes without a known auth middleware upstream (`passport`,
  `express-jwt`, custom `withAuth`).
- `res.redirect(req.<anything>)` → `redirect`.

### Fastify
- `fastify.register(plugin, { prefix })` without `onRequest` /
  `preHandler` auth hooks on the registered routes.
- Schema-less routes (no `schema:` in the route opts) skip input
  validation — flag as `config` (medium).

### Next.js (App Router / Pages Router)
- `app/api/**/route.ts`: any export of `GET/POST/PUT/DELETE` that
  doesn't `await auth()` (next-auth) or `getServerSession()`.
- Server Actions (`'use server'`): action with no auth guard
  inside.
- `redirect()` from `next/navigation` with user input → `redirect`
  finding.
- Middleware `matcher` config covering `/api/*` but missing the
  protected path → `authz` finding.

### NestJS
- Controllers without `@UseGuards(...)` on their public methods.
- DTOs without `class-validator` decorators on user-input fields.
- `@Query()` / `@Body()` typed as `any` → input-validation gap.

## Secondary signals — package.json hygiene

Examined by the `triage` agent (not a scanner-tool node):

- `scripts.{preinstall,install,postinstall,prepare}` that call any
  binary not from `node_modules/.bin/` — flag as `other` with
  `dependency-install-hook` label. (Real malware audit lives in
  sec-audit-deps; here we surface *the project itself* shipping
  install-time code.)
- `dependencies` with `"file:..."` or `"git+ssh:..."` refs to
  unaudited paths → `config`.

## Excluded patterns

Don't flag:
- Files under `*.test.ts`, `*.test.tsx`, `*.spec.ts`, `__tests__/`,
  `__mocks__/`.
- Build outputs (`dist/`, `build/`, `.next/`, `out/`).
- Type definitions only (`.d.ts`).
- Storybook (`*.stories.tsx`).

## Output

```json
{
  "scanner": "js",
  "subscanners": ["semgrep"],
  "json_paths": { "semgrep": ".../js.json" },
  "finding_count": 17
}
```

## See also

- `[[lang-generic]]` — always-on layer that complements this skill.
- `[[finding-taxonomy]]` — required mapping for normalised output.

## Scanners (machine-readable — consumed by run_lang_scanners + scan_health)

Deterministic scanner specs for this language. `run_lang_scanners` (a tool
node, no LLM) runs each `cmd` with `$SCAN_DIR` and `$WORKSPACE_DIR` in the
environment and cwd = workspace; `scan_health` reads `output` to verify
coverage. To add/adjust JS/TS scanning, edit this block — no DSL change.

<!-- iterion:scanners
[
  {"id":"semgrep-js","output":"js.json","cmd":"semgrep --config=p/javascript --config=p/typescript --config=p/nodejsscan --config=p/owasp-top-ten --json --output=$SCAN_DIR/js.json --metrics=off --quiet --exclude=node_modules --exclude='.iterion' --exclude='.sec-audit' --exclude='.works' --exclude=dist --exclude=build --exclude=.next --exclude='*.min.js' $WORKSPACE_DIR || true"}
]
-->
