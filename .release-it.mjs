import { HEADER, writerOpts } from './scripts/changelog-writer.mjs'

// JS rather than JSON because the conventionalcommits preset requires
// `commitPartial` to be a function — a Handlebars template string is rejected.
export default {
  git: {
    commitMessage: 'chore: release v${version}',
    tagName: 'v${version}',
    pushArgs: ['--follow-tags', '--atomic']
  },
  npm: {
    publish: false
  },
  github: {
    release: true,
    releaseName: 'v${version}'
  },
  hooks: {
    'after:bump': ['bash scripts/sync-chart-version.sh', 'bash scripts/build.sh']
  },
  plugins: {
    '@release-it/conventional-changelog': {
      preset: 'conventionalcommits',
      // Prepended to CHANGELOG.md in the release commit itself (release-it
      // stages it with `git add . --update`), so the file cannot drift from
      // the tags. Older majors are re-split by `task changelog:gen`.
      infile: 'CHANGELOG.md',
      header: HEADER,
      writerOpts
    }
  }
}
