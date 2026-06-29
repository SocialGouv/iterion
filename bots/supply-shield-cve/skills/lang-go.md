---
name: lang-go
description: |
  Go modules heuristic reference for sec-audit-deps. Covers go.mod
  / go.sum parsing, vendored module audit, govulncheck integration,
  and init() side-effect detection. Loaded when tech.ecosystems ∋
  gomod.
---

# lang-go — Go modules

Activated when `enumerate_deps` finds `go.mod` at workspace root.
Go modules are the only Go ecosystem we support — no GOPATH / dep /
glide.

## Enumeration

Source order:
1. **Vendored**: walk `vendor/modules.txt` and `vendor/**/*.go` for
   the per-package source.
2. **Module cache**: `$GOMODCACHE/<mod>/<ver>/` (typically
   `~/go/pkg/mod/`) when vendored isn't shipped.
3. **Manifest only**: parse `go.sum` for `(name, version, h1:hash)`
   triples; mark as "shallow" since we can't inspect source.

Checksum: from `go.sum` `h1:` field. Already SHA256 base64 — convert
to hex for cache consistency.

## Heuristic logic (signals emitted)

### `untrusted-replace`
- Parse `go.mod`. Emit for each `replace` directive whose target:
  - Is a local path (`./` or `../`).
  - Points to a fork (non-`google.golang.org/`, non-`golang.org/`,
    non-original-org) for security-sensitive modules: anything in
    `crypto/`, `golang.org/x/crypto`, `golang.org/x/oauth2`,
    `github.com/golang-jwt/jwt`, `github.com/lestrrat-go/jwx`.
- Evidence: `go.mod:<line>`.
- Score: 20.

### `init-side-effect`
- For each Go file in a dep, parse with `go/ast` and find
  `func init() { ... }` blocks.
- Look for calls to: `os.Exec*`, `os.StartProcess`, `net.Dial`,
  `http.Get`, `http.Post`, `os.WriteFile`, `os.Setenv`,
  `os.Create`, `syscall.*`.
- Skip when the package matches a known-safe pattern (test files
  `*_test.go`, generated files, `cmd/` mains of the dep itself).
- Evidence: `<file>:<line>:<call>`.
- Score: 25.

### `install-hook`
- Go has no install-time hooks per se, but `go generate` directives
  and `//go:build` constraints that pull in unusual files can be
  abused. Emit when a dep ships `//go:generate ...` referencing
  external commands.
- Score: 10 (informational).

### `eval-on-startup`
- Go doesn't have `eval`, but `reflect.Value.Call` invoked at
  init or in package-level vars can be similar. Flag when a
  package-level var initialisation calls `reflect.Call` /
  `reflect.MakeFunc`.
- Score: 25.

### `network-on-import`
- Caught by `init-side-effect` already (Go has no separate
  "import time" execution outside init / package vars). Don't
  emit twice — the catalog `init-side-effect` is the umbrella
  signal here.

### `binary-payload`
- `find <pkg-dir> -size +50k -type f -name '*.bin' -o -name '*.so'`
- Score: 15.

### `untrusted-replace` + `init-side-effect` combo
- When BOTH fire for the same package, bump merged score by 10
  (textbook trust-supply-chain abuse).

### `vuln-db-known`
- Run `govulncheck -json ./...` once at workspace root.
- For each vuln, emit with `osv_id`, `cvss`, `affected_module`,
  `affected_symbol`.
- Score: 20 / 35 by CVSS.

## Tool node skeleton

```bash
mkdir -p {{vars.scan_dir}}/heuristics
cd {{vars.workspace_dir}}
govulncheck -json ./... > {{vars.scan_dir}}/heuristics/govulncheck.json 2>/dev/null || true

go run - <<'GO' > {{vars.scan_dir}}/heuristics/go.json
// Pseudocode:
// 1. Parse go.mod for replace directives.
// 2. For each dep in go.sum or vendor/, run AST-based init() audit.
// 3. Cross-reference govulncheck.json.
// 4. Emit {"packages": [...], "errors": []}.
GO
```

(In practice the tool node uses `python3` + `subprocess` to invoke
`go run` for the AST audit. A future iteration could ship a small
helper binary.)

## AST audit detail

```go
// Pseudocode: walk all *.go files in a dep, parse with go/parser,
// for each FuncDecl where Name.Name == "init" or for
// package-level GenDecl with VAR token + ValueSpec with non-trivial
// Values, walk the body and flag dangerous calls.
```

The vendored case is the most reliable: we have all source. The
non-vendored case requires walking `$GOMODCACHE` which devbox /
sandbox may not have populated.

## Scope of "entry-point" code

For Go, "import time" = anything in:
- `init()` functions
- Package-level `var` initialisations
- `//go:generate` directives (not strictly runtime but the operator
  may have just run `go generate` so we treat it as in scope)

Function bodies that ONLY run when explicitly called are out of
scope (those are sec-audit-source territory).

## Out of scope

- C/cgo audit. Anything calling into C is outside this bundle's
  scope. Flag once with a `cgo-present` informational signal
  (score 5) but don't audit deeper.

## See also

- `[[malware-signals]]`
- `[[package-cache]]`
- `[[supply-shield-cve]]`

## Heuristic SCA scanner (machine-readable — consumed by run_eco_heuristics)

Deterministic. `run_eco_heuristics` (a tool node, no LLM) runs `cmd` with
`$SCAN_DIR` in the env and cwd = workspace, then parses `output`. To adjust
Go SCA, edit this block — no DSL change.

<!-- iterion:heuristics
[ {"id":"govulncheck","output":"govulncheck.json","cmd":"[ -f go.mod ] && command -v govulncheck >/dev/null 2>&1 && govulncheck -json ./... > $SCAN_DIR/govulncheck.json.tmp 2>/dev/null && mv $SCAN_DIR/govulncheck.json.tmp $SCAN_DIR/govulncheck.json || true"} ]
-->
