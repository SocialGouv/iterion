import assert from 'node:assert/strict'
import test from 'node:test'
import { JSDOM } from 'jsdom'
import { createMarkdownRenderer } from 'vitepress'
import { parse } from 'vue/compiler-sfc'
import { literalChangelogProse } from '../.vitepress/changelog-prose.mjs'

const md = await createMarkdownRenderer(process.cwd(), {
  config: (renderer) => renderer.use(literalChangelogProse),
})

for (const blank of ['', '\n']) {
  for (const eol of ['\n', '\r\n']) {
    test(`generated disclosure preserves literal prose (blank=${!!blank}, CRLF=${eol.length === 2})`, () => {
      const prose = 'The <tbody> and <body> contain `<uuid>` and `{{input.*}}`, & "quotes" and </details> as examples.'
      const source = `* Entry\n\n  <details><summary>why</summary>\n${blank}  ${prose}\n${blank}  </details>\n`.replaceAll('\n', eol)
      const html = md.render(source, { relativePath: 'changelog.md' })
      assert.deepEqual(parse(`<template>${html}</template>`).errors, [])
      const fragment = JSDOM.fragment(html)
      assert.equal(fragment.querySelectorAll('details').length, 1)
      assert.equal(fragment.querySelector('summary').textContent, 'why')
      assert.equal(fragment.querySelector('details').textContent, `why${prose}`)
      assert.equal(fragment.querySelector('details').querySelector('tbody'), null)
    })
  }
}

test('ordinary documentation keeps authored HTML in disclosures', () => {
  const source = '<details><summary>why</summary>\nUse <strong>bold</strong>.\n</details>\n'
  const fragment = JSDOM.fragment(md.render(source, { relativePath: 'guide.md' }))
  assert.equal(fragment.querySelector('strong').textContent, 'bold')
  assert.equal(fragment.querySelector('details').hasAttribute('v-pre'), false)
})
