# clawfold — folding claw-code-go into `pkg/claw`, with a proof per file

A move of ~140 k lines reviewed through one hash per file instead of trusted.

**Read it when** you plan, land, review or rebase the fold of claw-code-go into
this tree (epic #3153, stream MG), or when a branch still imports
`github.com/SocialGouv/claw-code-go` after the fold.

The tool is [`internal/clawfold`](../../../internal/clawfold/doc.go) behind
[`scripts/clawfold`](../../../scripts/clawfold/main.go): the review is of the
tool and of its plan, not of 650 files.

## The membership is computed, never listed

At the pin `vendor/modules.txt` holds (the membership's own source; a
`replace`d module is refused), read from a claw clone (default
`<primary checkout>/.repos/claw-code-go`) with `--no-replace-objects`, every
blob re-hashed against its id, and **the whole tree hashed against the `h1:`
go.sum holds** for that version — the checksum the Go checksum database
attests — so a rewritten object anywhere in the clone refuses the plan. Only
the module zip's files count, by `golang.org/x/mod/zip`'s own rules: no nested
module, no vendored package (under the root go.mod's go-version rule), no
symlink, no `.hg_archival.txt`; every file the zip leaves out is named as a
warning, with its reason. The plan writes nothing in iterion's tree; on a
partial clone git may fetch the pin's missing blobs:

1. **The core** is the vendored set — what iterion links, embeds included —
   and every one of its files must have the **same blob** at the pin, or the
   tool refuses: the membership source and the content source are one commit.
2. **Closure** over the claw imports of every Go file of every member package,
   **sources and tests**, parsed with `go/parser` (never a text search). The
   tests reach `internal/testutil`, `schematest`, `hookstesting`.
3. **Tests-only packages nested under a member** (`internal/api/providers`):
   nothing imports them, so no closure finds them.
4. **Non-Go files a member package owns** — the nearest ancestor directory
   holding Go is a member: the `//go:embed all:bundled_skills` tree, testdata —
   and whatever a member's `//go:embed` patterns name, `all:` honoured.
5. **Fixtures outside every package** that a member test reads through a
   relative path — a `"../…"` literal, or a `filepath.Join` of literals:
   `testdata/golden/` at the module root (a referenced directory comes whole).
6. At the root, only what the vendored set carries (`LICENSE`). A `go.mod` or
   `go.work` outside a data directory is refused: it would make its directory
   another module, which `go build ./...` then skips in silence.

A Go file under `testdata/`, `_x/` or `.x/`, or named `_x.go`/`.x.go`, is data
to the go tool: carried verbatim, never rewritten, named as a residual if it
spells the module path. A binary file counts 0 lines, as git counts it. Destination: `X` → `pkg/claw/X`; Go's `internal/` visibility holds
unchanged under `pkg/claw/internal/`.

## The rewrite and its proofs

Only **import specs** are rewritten, at the bytes of the literal:
`github.com/SocialGouv/claw-code-go[/…]` → `github.com/SocialGouv/iterion/pkg/claw[/…]`
(a sibling such as `claw-code-gopher` is not under the prefix). A comment or a
string that spells the path stays as it is and is **listed as a residual** —
`internal/api/identity.go:27` `clawModulePath` reads the build info for the
User-Agent: after the switch it reports `claw-code-go/dev`, which is the client
identity stream's call (MG-2), not the fold's.

The plan refuses a file that already imports under the destination (the
inverse would not be exact), an import literal spelled with an escape, and a
rewrite that changes what gofmt does to the file, either way; a source the
pin ships unformatted, and that stays so, is a warning, as is a Go data file
(CI's gofmt step reads it too).

Per file the plan records `src_blob`, `dest_blob`, the mode and the rewritten
lines. `verify` replays five proofs on the tree, each with its own wording:

| proof | the finding |
|---|---|
| the file's blob is the planned one | `blob X, the fold wrote Y` |
| un-rewriting it gives the pin's blob back | `unrewrite gives blob X, the pin holds Y` |
| it differs from the pin only on the rewritten lines | `differs … on lines […], the rewrite touches […]` |
| the owner execute bit (git's reading) matches the pin's mode | `mode …, the pin's is 100755` |
| git's index records that mode and blob | `the index records …` |

Anything under `pkg/claw` that no slice carries, a symbolic link included, is
a finding (`not in the plan`) and blocks `apply` and the switch.

**A reviewer needs neither the tool's hashing nor its word**: in a **fresh**
claw clone (a local one can carry replace refs), on the switch's **parent**
commit (after it the vendored set the plan reads is gone), copy `pkg/claw`, run
`clawfold unrewrite` on the copy, then `git hash-object --stdin-paths` must
print, file for file, the `src_blob` of `clawfold plan -json` — which is
`git ls-tree <pin>` of the clone.

## Slices

The fold lands in slices under a line cap (lines as git counts an added file).
**Assets first** — inert, verbatim, no dependency, packed under `-asset-cap`
(default: `-cap`). **Then the Go packages**, over the source+test import graph
with its strongly connected components contracted: an external test package
that imports back binds its packages into one landing (the `internal/api` block
below). A component lands when every dependency is in an earlier slice or the
same one; the packer takes the largest ready component that fits, ties by name,
so the plan depends on the census alone. A cap under the largest component is
refused with the **floor** named — a Go package cannot be cut across two pull
requests.

Each slice compiles and tests on what landed before it, and lands **unused**:
nothing in iterion imports `pkg/claw` until the switch. Unused is not invisible
to the gate — see below.

## Commands

```sh
task claw:fold:plan                      # membership, slices, switch, residuals, plan id; writes nothing
task claw:fold:plan -- -cap 18000 -json  # another cap; the per-file manifest
task claw:fold:verify -- -slice 3        # replay the proofs (no -slice: the whole plan, absences counted)
go run -mod=vendor ./scripts/clawfold apply -slice 3 -plan <id>
go run -mod=vendor ./scripts/clawfold apply -switch
go run -mod=vendor ./scripts/clawfold rewrite <paths…>    # and unrewrite: the bare rewrite, in place
```

- **`apply -slice` takes the plan id `plan` printed** — it hashes every
  slice's files and blobs — and refuses another (exit 2): the caps decide what
  slice K is. It also refuses a tree that is not a prefix of the plan.
- `apply` refuses a dirty `go.mod`/`go.sum`/`vendor/` (exit 2), like
  `vendor-check`: commit each slice before the next. A file with other bytes or
  another mode is a conflict, whichever slice holds it, and nothing is written.
  Each file lands through a rename, whole or not at all; a run cut short
  between the two may leave a `.clawfold-*` temporary file, which the next run
  names — remove it and that run resumes. A file already in place, on disk and
  in the index, is skipped, and a second `apply` writes nothing — re-running it
  on any branch is the rebase.
- After a Go slice `apply` runs `go mod tidy` + `go mod vendor` (with
  `GOWORK=off`; tidy needs the module proxy), prints the go.mod delta it made
  (a **version that moved** is named) and what the landing **owes** before its
  commit: `task map:gen`, and `task corpus:reading -- --write` when it adds
  `.md`/`.yaml`/`.yml`.
- `apply -switch` refuses (exit 2) a Go file the go tool compiles that git does
  not track — untracked or ignored alike —, a tracked Go file changed or
  missing on disk (a sparse checkout; `plan` then prints the slices without the
  switch), a changed file under `pkg/claw`, and a fold member git does not
  hold (exit 1 if one is hidden: not a prefix); it needs every
  slice landed and proven. It proves every consumer's rewrite before writing
  the first — a formatted consumer comes back **gofmt-sorted** (43 of 201 at
  this pin), proven as "gofmt of the inverse gives the file back", and refused
  if comments in its import block would be left behind — then tidy + vendor
  drop the module. It prints the go.mod delta and fails if the module is still
  required or still vendored. It leaves this tool and every nested module
  alone, and takes no cap and no plan id.
- `rewrite`/`unrewrite` skip `vendor/`, hidden directories and the go tool's
  data files, with the same proofs, all files proven before one is written.
- Exit 2 and 1 are the binary's; through `go run` or `task` both read as a
  failure, nothing finer.

## Measured at pin `71976d7241ee` (2026-11-12)

| | |
|---|---|
| packages | 58 — 54 vendored, 3 by import closure, 1 tests-only |
| Go | 503 files (239 tests), 103 910 lines, 358 import sites |
| assets | 147 files, 37 998 lines (123 embedded skill files, 19 of them 100755) |
| total | 650 files, 141 908 lines |
| bound block | `internal/api` + `httputil`, `openaiwire`, `sseutil`, `internal/permissions` — 12 195 lines |
| floor | `internal/tools`, 15 628 lines |
| cap 16 000 | 10 slices: assets 16000 · 16000 · 5998, then Go 15985 · 15092 · 15960 · 15555 · 10110 · 15628 · 15580 |
| beyond the cap | tidy+vendor add 14 792 lines with slice 4 (`jsonschema/v6` and the `x/text` it pulls), 323 with slice 5 (`grpc/test/bufconn`) |
| go.mod over the whole fold | the slices' tidy: 10 requirements indirect → direct at equal versions, + `jsonschema/v6`; the switch: − claw — no version moves |
| switch | 201 iterion files, 260 import sites, 12 path residuals |

The first estimate (12/11) counted Go only, with one line too many per file
(104 056), and missed the tests-only package, the root goldens and the 37 998
lines of assets `go:embed` requires: 7 slices at 16 000 hold for the Go alone,
not for the fold.

## What the gate says about each slice — MG-4's first decision

Rehearsed on the real tree, every slice landed in order, the switch last: each
Go slice builds, vets and tests on what landed before it, gofmt is clean,
`verify` proves 650 of 650, `git hash-object` of the unrewritten copy equals the
pin 650 of 650, and after the switch `go build ./...` and `go vet ./...` pass.
But **iterion's own gate turns red from slice 1**, and the byte-exact proof
forbids fixing claw's code inside a slice:

| from slice | red | the cause |
|---|---|---|
| 1 (assets) | `internal/mdcode` corpus golden | every landed `.md` is pinned — regenerated by `corpus:reading --write` |
| 4 | `pkg/git` caller guards | `runtime/lane/stale_{base,branch}.go`, then `context/git.go` (5), `commands/*_cmds.go` (7), `tools/skill_install.go` (9) |
| 4 | `pkg/repomap` map freshness | new packages — `task map:gen` |
| 4 | the required `golangci` check | 73 issues over the whole fold under this repo's config (48 staticcheck, 17 errcheck, 5 unconvert, 2 unused, 1 ineffassign) |
| 7 | `internal/envtrust` LookPath seam | `commands/{code,context,diagnostic}_cmds.go`, then `tools/{computer_use_exec,repl}.go` (9) |

So MG-4 needs a **slice 0** that admits `pkg/claw/` where `vendor/` already is
(the lint config, the tree-sweep guards, and `pkg/claw/** -text` beside
`vendor/** -text` in `.gitattributes`, so no eol rule rewrites a landed file),
each slice carrying the regenerations `apply` prints — an operator decision,
not this tool's. No gate runs `verify`
yet: during the landing it belongs in the train, and a committed manifest of
the plan would let CI replay the hash proofs without the clone.

The switch's path residuals keep naming the vendored tree the switch removes —
among them `pkg/platformcfg/platformcfg_infra_test.go`,
`internal/envtrust/routing_completeness_test.go`, three `pkg/backend/model`
tests and `pkg/dsl/ir/leaf_witness_test.go`. `clawfold plan` lists them all;
they are the switch's own work.
