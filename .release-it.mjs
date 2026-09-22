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
    'after:bump': ['bash scripts/sync-chart-version.sh', 'bash scripts/build.sh'],
    // The floors aligner runs in the one slot where package.json is bumped
    // AND CHANGELOG.md is written while nothing is staged yet: it rewrites
    // the syntax-floor pins the cut claims — inside this same release
    // commit, which the git plugin stages right after — and refuses,
    // aborting the release before that commit, a pin shape it cannot
    // realign or a floor whose word the notes just rendered do not carry
    // (#1154). At after:bump the conventional-changelog plugin has not
    // written the infile yet, so the aligner would judge notes that do not
    // exist on disk.
    'before:git:beforeRelease': ['go run ./cmd/release-floors --apply']
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
