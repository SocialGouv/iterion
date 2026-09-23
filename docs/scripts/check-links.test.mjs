// The link checker's own tests. It BLOCKS publishing, so a verdict it gets
// wrong either stops the site or lets a dead link through, and both failures
// are invisible from the outside — the checker reports "all targets resolve"
// either way. Every case here is one URL form the site really serves, or one
// the site really does not.
//
// Run: `task docs:links:test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'

import { BASE, BLOB, TREE, brokenLinks, collectDistFiles, repoResolver, trackedPaths } from './check-links.mjs'
import { PUBLIC_ROOT, publicSitePath } from '../.vitepress/public-links.mjs'

// A built site carrying exactly the pages the cases below link to. The files
// are real and the checker walks them: nothing here stands in for dist/.
function buildFixtureSite(hrefs) {
  const dist = mkdtempSync(join(tmpdir(), 'check-links-dist-'))
  const put = (rel, body) => {
    const abs = join(dist, rel)
    mkdirSync(dirname(abs), { recursive: true })
    writeFileSync(abs, body)
  }
  put('index.html', '<html><body>the site root</body></html>')
  // A page whose NAME carries dots. `probe.v1.2` is a version, not a file
  // extension, and only the built site can say which.
  put('probe.v1.2.html', '<html><body>a dotted page name</body></html>')
  // A page whose name is not ASCII: dist keys are the bytes readdir returns,
  // an href is percent-encoded.
  put('probé.html', '<html><body>an accented page name</body></html>')
  put('philosophy.html', '<html><body>philosophy</body></html>')
  put('guide/index.html', '<html><body>a directory page</body></html>')
  put('assets/style.css', 'body{}')
  put('page.html', `<html><body>${hrefs.map((h) => `<a href="${h}">x</a>`).join('\n')}</body></html>`)
  return dist
}

// Runs the checker over the fixture and returns the hrefs it flagged as dead.
function flagged(hrefs, { repoHas = () => true } = {}) {
  const dist = buildFixtureSite(hrefs)
  const distFiles = collectDistFiles(dist)
  return new Set(brokenLinks({ dist, distFiles, repoHas }).broken.keys())
}

// … and the ones it flagged as sent to github for a file the site serves.
function misrouted(hrefs, { repoHas = () => true } = {}) {
  const dist = buildFixtureSite(hrefs)
  const distFiles = collectDistFiles(dist)
  return new Set(brokenLinks({ dist, distFiles, repoHas }).misrouted.keys())
}

// Each row is a form the site SERVES. The checker rejecting one of these stops
// publishing over a link that works — measured on all five during #1508.
const resolves = [
  {
    name: 'a page whose name carries a dot that is not an extension',
    href: `${BASE}probe.v1.2`,
  },
  {
    name: 'a percent-encoded page name',
    href: `${BASE}prob%C3%A9`,
  },
  {
    name: 'a query string, which is not part of any file name',
    href: './philosophy?utm=1',
  },
  { name: 'a tel: URL', href: 'tel:+33123456789' },
  { name: 'a javascript: URL', href: 'javascript:void(0)' },
  { name: 'a data: URL', href: 'data:image/png;base64,iVBORw0KGgo=' },
  {
    // config.ts already recognises this form (`/^([a-z]+:)?\/\//i`); the two
    // files disagreeing is what reported it as "absolute without base".
    name: 'a protocol-relative URL naming another origin',
    href: '//example.com/x',
  },
  {
    // GitHub Pages 301s /iterion to /iterion/ — a redirect, not a 404.
    name: 'the site base without its trailing slash',
    href: BASE.replace(/\/$/, ''),
  },
  { name: 'a page under the base', href: `${BASE}philosophy` },
  { name: 'a directory page', href: `${BASE}guide/` },
  { name: 'an asset with a real extension', href: `${BASE}assets/style.css` },
  { name: 'a fragment on a page that exists', href: `${BASE}philosophy#why` },
  { name: 'a bare fragment', href: '#section' },
  { name: 'a mailto: URL', href: 'mailto:someone@example.com' },
  { name: 'an external site', href: 'https://example.com/x' },
]

for (const row of resolves) {
  test(`resolves: ${row.name}`, () => {
    assert.deepEqual([...flagged([row.href])], [], `the checker rejects ${row.href}, which the site serves`)
  })
}

// The other half: the checker must still say no. A fix that widens a predicate
// until nothing is ever flagged passes every case above.
// `reportedAs` is the key the report collapses the href to, where that differs
// from what the page wrote: the checker names the PATH, so one dead page linked
// with three different fragments is one line and not three.
const dead = [
  { name: 'a page that was never built', href: `${BASE}nope` },
  { name: 'a relative page that was never built', href: './nope' },
  { name: 'an absolute path outside the base', href: '/elsewhere/x' },
  { name: 'a directory with no index', href: `${BASE}assets/` },
  { name: 'a relative path climbing out of the site', href: '../../etc/passwd' },
  { name: 'a query string on a page that was never built', href: './nope?utm=1', reportedAs: './nope' },
  { name: 'a fragment on a page that was never built', href: `${BASE}nope#why`, reportedAs: `${BASE}nope` },
  { name: 'a percent-encoded page that was never built', href: `${BASE}nop%C3%A9` },
  { name: 'an escaped climb out of the site', href: '%2e%2e/%2e%2e/etc/passwd' },
]

for (const row of dead) {
  test(`flags: ${row.name}`, () => {
    assert.deepEqual(
      [...flagged([row.href])],
      [row.reportedAs ?? row.href],
      `the checker accepts ${row.href}, which 404s`,
    )
  })
}

test('a github blob link is judged by what git serves, not by what the disk holds', () => {
  const tracked = new Set(['docs/real.md'])
  const repoHas = (p) => tracked.has(p)
  assert.deepEqual([...flagged([`${BLOB}docs/real.md`], { repoHas })], [])
  assert.deepEqual([...flagged([`${BLOB}docs/untracked.md`], { repoHas })], [`${BLOB}docs/untracked.md`])
})

// ---------------------------------------------------------------------------
// The github half, against a real repository.

function fixtureRepo() {
  const root = mkdtempSync(join(tmpdir(), 'check-links-repo-'))
  const git = (...args) =>
    execFileSync('git', ['-C', root, '-c', 'user.email=t@example.com', '-c', 'user.name=t', '-c', 'commit.gpgsign=false', ...args], {
      encoding: 'utf8',
    })
  const put = (rel, body) => {
    const abs = join(root, rel)
    mkdirSync(dirname(abs), { recursive: true })
    writeFileSync(abs, body)
  }
  // `trunk`, deliberately: the checker must not need a ref called `main`.
  execFileSync('git', ['-c', 'init.defaultBranch=trunk', 'init', '-q', root])
  put('.gitignore', 'ignored/\n')
  put('docs/tracked.md', '# tracked\n')
  put('docs/untracked.md', '# untracked, but on the disk\n')
  put('ignored/file.md', '# gitignored, but on the disk\n')
  mkdirSync(join(root, 'docs', 'empty'), { recursive: true })
  git('add', '.gitignore', 'docs/tracked.md')
  git('commit', '-qm', 'fixture')
  return root
}

test('trackedPaths answers for the files git serves and for their directories', () => {
  const paths = trackedPaths(fixtureRepo())
  assert.ok(paths.has('docs/tracked.md'), 'a tracked file is absent')
  assert.ok(paths.has('docs'), 'the directory of a tracked file is absent, so a tree URL to it would be flagged')
  // Each of these exists on the disk and 404s on github.com. existsSync
  // cannot tell them from the tracked file; that was the defect.
  assert.ok(!paths.has('docs/untracked.md'), 'an untracked file is reported as served')
  assert.ok(!paths.has('ignored/file.md'), 'a gitignored file is reported as served')
  assert.ok(!paths.has('ignored'), 'a gitignored directory is reported as served')
  // git cannot track an empty directory, so github.com serves no page for it.
  assert.ok(!paths.has('docs/empty'), 'an empty directory is reported as served')
})

test('repoResolver refuses a path that is on the disk and not in git', () => {
  const repoHas = repoResolver(fixtureRepo())
  assert.equal(repoHas('docs/tracked.md'), true)
  assert.equal(repoHas('docs/untracked.md'), false, 'the resolver answers from the disk, not from git')
  assert.equal(repoHas('docs/empty'), false)
})

// The CI checkout is `actions/checkout` at depth 1: it has the commit, not the
// history, and not necessarily a ref named after the default branch. A checker
// that asked git for `main:<path>` would flag every github link in that job —
// a false positive in a tool that blocks, which is the failure this ticket is
// about.
test('trackedPaths works in a shallow clone that has no default-branch ref', () => {
  const origin = fixtureRepo()
  const clone = join(mkdtempSync(join(tmpdir(), 'check-links-clone-')), 'c')
  execFileSync('git', ['clone', '-q', '--depth=1', '--branch', 'trunk', `file://${origin}`, clone])
  const refs = execFileSync('git', ['-C', clone, 'for-each-ref', '--format=%(refname)'], { encoding: 'utf8' })
  assert.ok(!refs.includes('/main'), `the clone has a main ref after all, so this case proves nothing:\n${refs}`)
  const paths = trackedPaths(clone)
  assert.ok(paths.has('docs/tracked.md'), 'the shallow clone answers nothing — the checker would flag every github link')
})

// A checker that cannot ask git must SAY so. Falling back to the disk is the
// defect wearing a different hat, and it would report success on a run that
// certified nothing.
test('a repository git cannot read is an error, never a fallback to the disk', () => {
  const notARepo = mkdtempSync(join(tmpdir(), 'check-links-plain-'))
  writeFileSync(join(notARepo, 'onDisk.md'), '# on the disk, in no repository\n')
  assert.throws(() => trackedPaths(notARepo), /tracked files/)
})

// ---------------------------------------------------------------------------
// docs/public/ — served from the site ROOT, so the path the source writes is
// not the path the reader gets.

test('publicSitePath maps the public tree onto the site root', () => {
  assert.equal(publicSitePath(`${PUBLIC_ROOT}/comparatifs/index.html`), '/comparatifs/index.html')
  assert.equal(publicSitePath(`${PUBLIC_ROOT}/og.png`), '/og.png')
  assert.equal(publicSitePath(PUBLIC_ROOT), '/')
})

test('publicSitePath answers for a path SEGMENT, not for a string prefix', () => {
  // `docs/publications/x` is not inside `docs/public/`, and a startsWith on
  // the bare name would say it is.
  assert.equal(publicSitePath('docs/publications/x.md'), null)
  assert.equal(publicSitePath('docs/public-notes.md'), null)
  assert.equal(publicSitePath('docs/dsl.md'), null)
  assert.equal(publicSitePath('pkg/repomap/repomap.go'), null)
})

// The canary for the rewrite rule in config.ts. The site ships these files
// itself, so a page of the site linking the github copy walks the reader out
// of the site for a file the same build just published — which is what the
// page source says when the public/ rule stops firing.
test('a github link to a file the site serves from public/ is reported as misrouted', () => {
  const href = `${BLOB}${PUBLIC_ROOT}/comparatifs/index.html`
  assert.deepEqual([...misrouted([href])], [href])
  // Not "broken": it resolves on github.com. Calling it broken would send the
  // author hunting for a missing file.
  assert.deepEqual([...flagged([href])], [])
})

test('a github link outside public/ is not misrouted', () => {
  const href = `${BLOB}pkg/repomap/repomap.go`
  assert.deepEqual([...misrouted([href], { repoHas: () => true })], [])
})
