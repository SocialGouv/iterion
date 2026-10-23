# Machine-dependent tests — annotate, measure, re-check

A machine-dependent failure stays visible. Record the test, machine class,
evidence (log or SHA), disposition and next re-check date. Keep the assertion;
neither deleting the test nor silently skipping it turns an unknown result
green. A missing prerequisite produces a named, fast refusal and an explicit
unverified result in the gate report. A rerun records its elapsed time and
environment alongside the first failure, not instead of it.

Fix the class: inventory every caller, isolate the resource at a shared helper,
and prove that restoring the defect reddens the witness on its own assertion.
Process fixtures own their scratch directories, HOME, configuration and child
lifetimes. Network access is not a remedy for an unexplained hang; see
[the hermetic gate decision](../../adr/137-local-gate-hermetic-by-default.md).

## Register

| Test / check | Machine class | Evidence (log / SHA) | Disposition | Re-check |
|---|---|---|---|---|
| `TestLaunchDetached_NamesTheDroppedOverrides` | Operator workstation with an installed iterion outside PATH | [`e40def071`](https://github.com/SocialGouv/iterion/commit/e40def0715d8a3c1841c504c984c1f4ddd1a158b), including its mutation record | **Corrected, verified 2026-10-24**: `proc.SetInstallPathCandidates` makes the fixed install tier injectable; the witness empties that tier and pins HOME and ITERION_BIN. | 2026-10-31, and on binary resolver changes |
| `TestRepoRootFindsTheNearestRealGitEntry` | Operator machine with a foreign, empty `/tmp/.git`, recreated by an unidentified process | Operator observation recorded in the [#2585 mission](https://github.com/SocialGouv/iterion/issues/2585), baseline `e40def071`; no archived log supplied | Environment parasite, creator unresolved. Inspect ownership/type/content; remove **only the confirmed empty directory** with `rmdir /tmp/.git` before taking a verdict. Record the purge and rerun; a refusal from `rmdir` stops the purge. Never recursively delete it or weaken repository-discovery assertions. | 2026-10-31, and before the next affected verdict |
| `🧪 Tests` workflow, queued jobs | Saturated ARC runner pool | Operator observation recorded in the [#2585 mission](https://github.com/SocialGouv/iterion/issues/2585): runs remain queued for hours; no individual run URL supplied | Infrastructure delay, **no test verdict**. Attach the run URL, SHA, queued/start timestamps and runner class on recurrence. Measure queue wait separately from execution; use the [queue runbook](../../merge-policy.md#measuring-the-queue). Missing/cancelled jobs never count as passing. | 2026-10-31, and at the next landing |

Re-check on 2026-10-24, `e40def071` plus the #2585 working diff: the jailed
`task check` reproduces the `/tmp/.git` failure. `stat` reports an empty
directory owned by `jo`, mode `0555`; `rmdir /tmp/.git` refuses with
`Device or resource busy`. The verdict remains red in this environment;
no recursive removal or forced unmount substitutes for the failed purge.

## Worked example: an installed binary defeats an empty PATH

`TestLaunchDetached_NamesTheDroppedOverrides` expects a detached launch to
refuse after logging dropped overrides. Emptying PATH alone cannot prove that:
the resolver also searches fixed installation paths, including the operator's
home. The fix supplies an empty install-candidate seam, an owned HOME and an
empty ITERION_BIN. The production resolver and its sandbox caller share that
seam; a second resolver would reopen the class.

The mutation recorded in `e40def071` makes the seam ineffective and removes
the HOME pin on a machine with an installed binary. The test then fails its
own expected-refusal assertion. A process leak warning is supporting evidence,
not that assertion. Re-check on both a machine with the installation and an
isolated worker, recording the exact SHA and elapsed time.
