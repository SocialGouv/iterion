# Release and changelog — how the cut reaches `main`

Floors, mirrors, refused cuts and the changelog pipeline, for the change that
ships or reviews one ([merge and gate](review-and-merge.md)).

## Release and changelog

- **Floor pins** — a syntax floor (`bundle.SyntaxFloors`:
  `parser.ProfileSince`, `ImportSince`, `ContractSince`,
  `bundle.ToolAliasesSince`) is pinned by the lot that ships the syntax to
  the next minor above the release main carries — a guess written before the
  number is known. The cut writes the real one: release-it's
  `before:git:beforeRelease` hook runs `go run ./cmd/release-floors --apply`
  ([internal/floorsalign](../../../internal/floorsalign)) in the one slot where
  `package.json` is bumped and `CHANGELOG.md` is rendered while nothing is
  staged, so every pin the cut claims — strictly above the previous release,
  measured there and not against the version being cut, since a major bump
  leaves the pin below it — is rewritten to the version being released
  **inside the `chore: release` commit itself**. A pin already at a released
  version is left alone. Every other shape aborts the release before that
  commit: a pin that is not the next minor above the previous release (an
  over-pin, or one a release overtook), a pin naming a release never cut, an
  unorderable pin, a floor with no entry in `releaseNotesMarkers`, and — the
  #1154 shape — a floor whose word the notes just rendered do not carry,
  which is what a `feat` merged under a non-conventional subject leaves
  behind. The rule is `bundle.HoldSyntaxFloor`, the *same* predicate
  `TestSyntaxFloorsNameReleasesThatExist` runs on every tree — the cut runs
  the test's predicate plus the resolution its rewrite needs (the pin's
  identifier declared in exactly one non-test file under `pkg/`, bound to a
  plain release literal), so it can refuse what the test lets pass, never
  the reverse. Either refusal is loud, and `devbox run -- task
  release:floors` previews the whole verdict on any checkout and writes
  nothing (`--apply` belongs to the hook and refuses to run unless
  `package.json`'s version differs from the one HEAD carries). The hatch is
  the marker table's exemption (an empty marker with its reason beside it,
  `parser.ProfileSince[2]` the precedent): an exempted floor realigns
  through notes that say nothing about it.
- **After a realigned cut, the mirrors.** The cut moves the CONSTANT, and
  whatever spells the number out has to follow. Two guards hold that, and
  both are required checks: the floor tests derive their expectations from
  the constants rather than repeating them, and
  `TestEveryShippedBundleDeclaresAFloorThatReachesWhatItNeeds` (pkg/bundle)
  walks the 41 shipped `bots/*/` and `examples/*/` manifests and refuses one
  whose `requires.iterion` no longer reaches what its own sources need —
  where `iterion validate` only warns (C252 is a warning, exit 0) while the
  push admission refuses the same bundle with a 409. What neither guard
  covers is a manifest left one release too HIGH after a downward
  realignment: that over-declares (the bundle refuses runners that could
  read it) and nothing says so, because no arithmetic can tell an intended
  floor from a stale one.
- **A refused cut leaves the bump behind.** The hook fails before release-it
  arms its rollback, so `package.json`, `CHANGELOG.md` and
  `charts/iterion/Chart.yaml` stay modified and a second local run reports
  "Working dir must be clean". Restore them
  (`git checkout -- package.json CHANGELOG.md charts/iterion/Chart.yaml`)
  before retrying. CI is unaffected: `version.yml` runs on a fresh checkout
  and nothing was committed or pushed.
- **tests.yml** — on push/PR: gofmt, go vet, unit tests, e2e tests
- **release.yml** — on git tags (v*): multi-platform builds (linux/darwin/windows × amd64/arm64), GitHub release
- **version.yml** — conventional changelog via release-it, version from `package.json`.
  A merged PR releases only while no merge group is in flight — the release
  commit is a direct push to `main`, which rebuilds every group the queue is
  building — so the merge that drains the queue releases the whole burst; the
  nightly and a `workflow_dispatch` release regardless
  ([releases and the queue](../../merge-policy.md#releases-and-the-queue)).
  release-it writes the new section into [CHANGELOG.md](../../../CHANGELOG.md) as part of the
  release commit itself (`infile` + `git add . --update`), so the file cannot drift
  from the tags — never hand-edit it. It holds the **current major only**; earlier
  ones are archived under [docs/changelog/](../../changelog) because GitHub stops
  rendering markdown past 512 KB. Each entry carries a collapsed `why` excerpt taken
  from the commit body — the rendering lives in
  [scripts/changelog-writer.mjs](../../../scripts/changelog-writer.mjs), shared by release-it
  ([.release-it.mjs](../../../.release-it.mjs)) and the regenerator (`task changelog:gen`), so
  a rebuilt section is byte-identical to a released one. Re-run `task changelog:gen`
  after a major bump, or when it warns the file is nearing the ceiling.
