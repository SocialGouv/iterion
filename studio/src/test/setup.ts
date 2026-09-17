// Vitest setup, applied to every test file (vite.config.ts `test.setupFiles`).
//
// Testing Library's async queries — findBy*, waitFor — give up after 1 s by
// default. That is a wall-clock budget, not a work budget: it is exceeded by a
// loaded machine rather than by a defect, and when it is, the failure reads
// exactly like a real one ("Unable to find role=..."). Measured on CI: one file
// out of 239 red on a 13m50s job, on a head whose only change was a markdown
// file; the same tests pass locally in ~200 ms. Reproduced by starving this
// very setting to 1 ms, which reproduces the CI error verbatim.
//
// 3 s buys 3x headroom and stays under Vitest's 5 s per-test timeout, so a
// genuinely missing element still fails with Testing Library's readable
// message rather than an opaque test timeout (the slowest passing test in this
// suite is ~890 ms, leaving ~1.1 s of margin).
//
// Guarded on `document` because 134 of the 239 files run in the fast Node
// environment (vite.config.ts) — only DOM files opt into jsdom with a per-file
// `// @vitest-environment jsdom`. The import is dynamic to spare those 134
// files a module load they would never use; it is NOT a safety guard —
// @testing-library/dom builds throwing stubs behind this same predicate rather
// than failing at import.
//
// Does not govern `vi.waitFor`: Testing Library's fake-timer detection tests
// `typeof jest !== "undefined"`, always false here, so prefer Testing Library's
// own `waitFor` in DOM tests. For the same reason this setting is inert under
// `vi.useFakeTimers()`, where a Testing Library `waitFor` runs on real timers
// that the test has frozen and dies at Vitest's 5 s timeout instead.
if (typeof document !== "undefined") {
  const { configure } = await import("@testing-library/dom");
  configure({ asyncUtilTimeout: 3000 });
}
