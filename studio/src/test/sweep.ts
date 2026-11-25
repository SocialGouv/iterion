// The time bound of a whole-tree sweep: a test that reads every source module
// of the studio — parses each one on the TypeScript AST, walks its JSX, scans
// its text — and pins what it finds. Its cost is the size of the tree, not the
// work of one assertion, and it grows with every module the studio gains.
//
// The default per-test bound (vite.config.ts, 15 s) is sized for one unit of
// behaviour. A sweep crosses it on a loaded CI runner with no defect at all:
// broken-classes' theme allowlist ran 1.8 s unloaded and 16.9 s on a loaded
// ARC pod (x9.4); the slowest sweep, lostRuns' pinned transport calls, runs
// 3.4 s unloaded, so ~32 s at that ratio. 60 s keeps twice that headroom. A
// test only reaches this bound when it is going to fail anyway, and the DOM
// tests that share a file with a sweep keep their readable failure: Testing
// Library's 3 s budget (setup.ts) still expires first.
//
// Every module that globs the source tree raw calls boundSweep() at module
// scope, so every test file that loads the tree — directly or through
// __tests__/a11y/_scanner.ts — registers its tests under this bound:
// Vitest reads testTimeout when `it` registers a test, and imports evaluate
// before the importing file's body. Vitest restores the configured bounds
// after each file, so the raise never leaks into a file that loads no tree.
// The guard in sweep.test.ts holds both halves: the bound a sweep test
// actually runs under, and the call beside every tree glob.
import { vi } from "vitest";

export const SWEEP_TIMEOUT_MS = 60_000;

/** Raises the test and hook bounds of the current test file to SWEEP_TIMEOUT_MS. */
export function boundSweep(): void {
  vi.setConfig({ testTimeout: SWEEP_TIMEOUT_MS, hookTimeout: SWEEP_TIMEOUT_MS });
}
