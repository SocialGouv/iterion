# Representative pilot measurements for #1165

Threshold version 3 was frozen in commit `3d933d067` and the fixture in
`9e627ac0e` before these measurements. Versions 1 and 2 were discarded
because the Shorts reference was initially a batch node and then the wording
confused fake item-handler calls with real paid effects. The
[threshold document](public-contracts-pilot-thresholds.md)
names the three source files, SHA-256 identities, equivalence rules and latency
ceiling. No file in Shorts, Town or Tabarria was changed or run.

The test `TestPublicContractsRepresentativePilots` used Go 1.26.2 through
`devbox run`, a private filesystem store per run, a deterministic fake executor
and three runs per case. The following are median wall times in milliseconds:

| Slice | Jobs | Legacy | Native | Calls (prepare/item/collect) | Peak item jobs | Result |
| --- | ---: | ---: | ---: | --- | ---: | --- |
| Shorts unit dispatch | 0 | 86.0 | 121.0 | 1/0/1 | 0 | pass |
| Shorts unit dispatch | 1 | 125.8 | 160.2 | 1/1/1 | 1 | pass |
| Shorts unit dispatch | 4 | 206.2 | 330.8 | 1/4/1 | 2 | pass |
| Town epic image stage | 0 | 86.4 | 119.3 | 1/0/1 | 0 | pass |
| Town epic image stage | 1 | 123.6 | 156.9 | 1/1/1 | 1 | pass |
| Town epic image stage | 4 | 190.6 | 240.7 | 1/4/1 | 2 | pass |
| Tabarria still stage | 0 | 83.2 | 119.2 | 1/0/1 | 0 | pass |
| Tabarria still stage | 1 | 126.4 | 163.8 | 1/1/1 | 1 | pass |
| Tabarria still stage | 4 | 189.7 | 243.0 | 1/4/1 | 2 | pass |

Each paired run produced the same ordered identifiers (`unit-i-ready`,
`epic-view-i-ready`, or `still-i-ready`) and the same joined summary. The
native run committed both required public products. All success cases had zero
retries, exactly N fake item-handler calls and zero real paid-effect calls. For
four jobs the harness holds the first two until both are admitted, so the
observed peak of two tests
actual parallel admission rather than relying on scheduler timing. The
predeclared ceiling `max(2 × legacy median, legacy median + 100 ms)` passed in
all nine cases in the successful version-3 run. The native coordinator was
**slower in all nine** on these 10-ms fake jobs, by roughly 26–60%; its durable
admission/publication writes have visible overhead at this scale. This test
gives no evidence of lower
model cost or faster full workflows. Model tokens and monetary cost are
**unknown**, because no model was called.

The race-instrumented run verified all 13 named cases without skips, including
the same outputs, invocation counts and parallel-admission bounds. Its timings
are logged but are not compared with
the ordinary-run latency ceiling: race instrumentation changes the two
engines' filesystem and scheduling costs unequally. The ordinary run keeps
the frozen ceiling as a required assertion.

The first version-3 run exceeded the latency ceiling in two zero-job cases
while the host was heavily loaded; its semantic checks passed. A fresh,
unchanged version-3 run passed all nine cases, and the 13-case manifest
verified execution without skips. This shows that the wall-time guard is
sensitive to host contention. It is not evidence of reliable performance
under load or a reason to raise the threshold after the fact.

The fixtures reproduce only the selected fan-out/convergence skeletons. The
legacy fixture uses its existing `fan_out_each` and an explicit fake collector;
the native fixture uses a typed array-to-scalar map and public exports. The
source bots' actual subbots, family routing, review loops, human gates,
idempotent media production, file validation, and project persistence are not
executed or certified here. Those require verified adapters/composition and
separate full-pipeline evaluation before any migration recommendation. The
three project repositories remain untouched.

Reproduction:

```bash
devbox run -- go test ./pkg/runtime -run TestPublicContractsRepresentativePilots -count=1 -v
devbox run -- go test -race -json ./pkg/runtime -run TestPublicContractsRepresentativePilots -count=1 > /tmp/iterion-pilots.jsonl
devbox run -- node scripts/verify-port-tests.mjs pkg/runtime/ports_pilot_cases.json /tmp/iterion-pilots.jsonl
```
