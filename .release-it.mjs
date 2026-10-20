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
    //
    // The mdcode golden runs in the same slot for the same reason: the cut
    // rewrites CHANGELOG.md and charts/iterion/Chart.yaml, both pinned by
    // internal/mdcode's committed-corpus golden, so the golden regenerates
    // here and lands inside the release commit too — otherwise every
    // release reopens the red the golden exists to make deliberate (#2600).
    // It runs FIRST: the regeneration moves no pin, so its failure aborts
    // the cut with the floors still where HEAD left them. -v because cmd/go
    // swallows a PASSING package's output without it, and the rewrite's
    // stderr announcement must reach the release log; the last line adds
    // the golden's diff stat to that log.
    'before:git:beforeRelease': [
      'MDCODE_UPDATE_GOLDEN=1 go test ./internal/mdcode/ -run TestTheCommittedCorpusKeepsItsReading -count=1 -v',
      'go run ./cmd/release-floors --apply',
      'git diff --stat -- internal/mdcode/testdata/committed-reading.txt'
    ]
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
