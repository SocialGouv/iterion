---
name: test-types
description: What each test type means (unit / integration / e2e / property / contract / snapshot / smoke / performance) and how to write + run + measure each idiomatically per ecosystem (Go, JS/TS, Python, Rust, Java/JVM, Ruby, others). Read when deciding which tests to add or how to write them in this repo's stack.
---

# test-types — the taxonomy and the per-stack "how"

First match the **repo's existing conventions** — read a few existing
tests and copy their structure, naming, runner, and assertion library.
The guidance below is the fallback when the repo has no precedent for the
type you are adding. Always prefer the house style over this skill.

## The taxonomy (stack-independent)

- **Unit** — one function/method/class in isolation; collaborators that
  cross an I/O boundary (network, disk, clock, randomness) are faked.
  Fast, deterministic, many small cases. The bread and butter.
- **Integration** — two or more real components together across a real
  boundary: a handler + a real (or test-container) DB, a repository + the
  filesystem, two services over a real transport. Asserts the seam works,
  not just each side in isolation.
- **End-to-end (e2e)** — the system from the outside: a real CLI
  invocation, an HTTP request through the running server, a browser
  driving the UI. Only add when the repo already has an e2e harness to
  plug into; do not stand one up from scratch unasked.
- **Property-based** — assert an invariant over many generated inputs
  (round-trip `decode(encode(x)) == x`, ordering, idempotence) rather than
  hand-picked examples. Great for parsers, serializers, data structures.
- **Contract** — verify a producer/consumer agree on an interface schema
  (API request/response shape, message format). Pact-style or
  schema-validation.
- **Snapshot / golden** — compare output to a stored reference. Only
  legitimate when the reference has been **verified correct** (see
  [[test-coverage]] — a snapshot of unverified current output is a
  façade). Iterion's own `pkg/botreplay` golden framework is an example
  of disciplined golden testing.
- **Smoke** — a tiny "does it start / does the happy path work at all"
  check, often for CI gating.
- **Performance / benchmark** — assert latency/throughput or guard
  against regressions; usually separate from the correctness suite.

Pick the **smallest type that meaningfully asserts the behaviour**. A
unit test that exercises the real logic beats an integration test that
mostly checks wiring, and both beat an e2e test for verifying a pure
function.

## Per-ecosystem how-to (fallbacks)

Detect the stack from its markers; honour any pinned toolchain
(devbox / asdf / mise / nvm / lockfile) — see [[verify-tests]].

### Go (`go.mod`)
- **Layout:** `foo_test.go` beside `foo.go`, `package foo` (white-box) or
  `package foo_test` (black-box, preferred for public API).
- **Unit:** table-driven is the idiom —
  `tests := []struct{name string; in …; want …}{…}` then
  `for _, tt := range tests { t.Run(tt.name, func(t *testing.T){ … }) }`.
  Use `t.Errorf`/`t.Fatalf`; the stdlib `testing` package only — match
  the repo (iterion explicitly uses no assertion framework). Use
  `t.TempDir()` for filesystem isolation, `httptest` for HTTP handlers,
  interfaces + fakes for boundaries.
- **Run:** `go test ./path/...` ; per-package; whole module `go test ./...`.
- **Coverage:** `go test -cover ./...` ; profile
  `go test -coverprofile=cover.out ./...`, then `go tool cover -func=cover.out`
  (two commands: in a gate, a chain hides its first failure).
- **Property:** `testing/quick`, or native fuzz `func FuzzX(f *testing.F)`.
- **Bench:** `func BenchmarkX(b *testing.B)`.

### JavaScript / TypeScript (`package.json`)
- **Runner:** detect from devDeps/config — Vitest (`vitest.config`),
  Jest (`jest.config`), `node:test`, Mocha, Playwright (e2e),
  Cypress (e2e). Pick the package manager from the lockfile
  (`pnpm-lock.yaml`→pnpm, `yarn.lock`→yarn, `package-lock.json`→npm).
- **Layout:** `*.test.ts` / `*.spec.ts` beside source or under
  `__tests__/` / `test/` — match the repo.
- **Unit:** `describe/it` + `expect(...).toBe/toEqual/toThrow`; mock at
  module/network boundary (`vi.mock` / `jest.mock`, MSW for HTTP). Use
  `it.each([...])` for parametrised cases.
- **Run:** the project's `test` script (`<pkgmgr> test`), or
  `npx vitest run` / `npx jest`.
- **Coverage:** `vitest run --coverage` / `jest --coverage`.
- **e2e:** Playwright (`npx playwright test`) / Cypress — only if present.

### Python (`pyproject.toml` / `setup.py` / `requirements.txt`)
- **Runner:** pytest (most common) or stdlib `unittest`. Honour the env
  manager (poetry / uv / pipenv / venv).
- **Layout:** `tests/test_*.py` or `test_*.py` beside source.
- **Unit:** plain `def test_x():` + `assert`; `@pytest.mark.parametrize`
  for cases; `pytest.raises(Err)` for errors; `monkeypatch` /
  `unittest.mock` at boundaries; `tmp_path` fixture for files.
- **Run:** through the repo's environment manager — `uv run pytest path/`,
  `poetry run pytest path/` (a bare `pytest` needs that environment active).
- **Coverage:** `pytest --cov=<pkg>` (pytest-cov) or `coverage run -m pytest`.
- **Property:** Hypothesis (`@given(...)`).

### Rust (`Cargo.toml`)
- **Unit:** `#[cfg(test)] mod tests { #[test] fn … { assert_eq!(…) } }`
  in the same file. **Integration:** files under `tests/`.
- **Run:** `cargo test`. **Coverage:** `cargo llvm-cov` / tarpaulin if
  present. **Property:** `proptest` / `quickcheck`.

### JVM (`pom.xml` / `build.gradle`)
- JUnit 5 (`@Test`, `assertEquals`, `assertThrows`,
  `@ParameterizedTest`) + Mockito; AssertJ if the repo uses it.
- **Run:** `mvn test` / `./gradlew test`. **Coverage:** JaCoCo.

### Ruby (`Gemfile`)
- RSpec (`describe/it/expect`) or Minitest — match the repo.
- **Run:** `bundle exec rspec` / `rake test`. **Coverage:** SimpleCov.

### Anything else
Read the existing tests and CI config. Replicate the runner, the
assertion style, and the file layout the repo already uses. If the repo
has zero tests, introduce the single most idiomatic runner for its stack
and wire the minimal config its ecosystem expects.

## Universal rules (every stack)

- Put new tests where the repo keeps its tests; name them as the repo
  names them.
- Use the repo's existing assertion/mocking libraries — do not add a new
  test dependency unless the repo has none and one is required.
- Isolate I/O: temp dirs, in-memory or test-container DBs, fake clocks,
  seeded randomness. A flaky test is worse than no test.
- Every test must satisfy the **mutation test** in [[test-coverage]]:
  break the code → the test fails.
