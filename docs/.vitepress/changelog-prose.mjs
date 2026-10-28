// A generated "why" excerpt is commit prose, not executable Vue markup.
// Preserve the native disclosure while displaying source tags and DSL literally.
export function literalChangelogProse(md) {
  md.core.ruler.before('block', 'literal_changelog_prose', (state) => {
    const page = state.env?.relativePath || ''
    if (page !== 'changelog.md' && !/^changelog\/v[0-9]+\.md$/.test(page)) return
    state.src = state.src.replace(
      /^([ \t]*)<details><summary>why<\/summary>[ \t]*\n([\s\S]*?)^[ \t]*<\/details>[ \t]*$/gm,
      (_match, indent, prose) => `${indent}<details v-pre><summary>why</summary>${md.utils.escapeHtml(prose.trim())}</details>`,
    )
  })
}
