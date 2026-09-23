---
name: stack-go
description: Go extractors for the assessment — the declared toolchain version, the deployable commands, and the HTTP entrypoints, each emitted as JSON by a deterministic script carried in this file. Loaded when the survey names the `go` stack.
---

# stack-go — what an assessment reads out of a Go tree

Detection signals: a `go.mod` at the workspace root or in a subdirectory, and
`*.go` files beside it.

The workflow runs the extractors declared at the bottom of this file. Each one
writes one JSON document to standard output, which the runner captures under
`$SCRATCH_DIR`; the coverage gate then checks that the file exists and is
non-empty. A script that exits 0 and writes nothing is a silent coverage gap,
which is why the artefact is what gets verified rather than the exit code.

Every script receives `WORKSPACE_DIR`, `SCRATCH_DIR` and `BASE_SHA` in its
environment and runs with the workspace as its working directory.

## What each extractor is responsible for

- **`modules`** — the declared toolchain version (the `go` directive of every
  `go.mod`) and the module list. Read from the DECLARATION, never from
  whatever toolchain happens to be installed on the machine running the
  assessment: the question is what the repository asks for.
- **`commands`** — the deployable artefacts, counted as the directories
  holding a `package main`. A heuristic with a stated shape: it over-counts a
  repository whose tooling lives beside its product, and the survey's own
  `deployable` declarations are what an operator corrects it with.
- **`entrypoints`** — the HTTP surface, counted from the route registrations
  the standard library and the common routers use. Test files and vendored
  trees are excluded. The count is of REGISTRATIONS, so a route registered in
  a loop counts once.

## Reading the result

These three files feed a published measurement, so their failure modes matter
more than their convenience. An empty `entrypoints` result on a repository
that clearly serves traffic means the routing is built somewhere this pattern
set does not reach — configuration, code generation, a framework not listed.
That is an observation for the survey's `notes`, not a zero to publish
quietly.

Adding a router to the pattern set is an edit HERE. It is never an edit to the
workflow, which reads the block below and knows nothing about Go.

## Extractors

<!-- iterion:extractors
[
  {"id":"modules","output":"go-modules.json","emits":["declared_versions"],"interpreter":"python3"},
  {"id":"commands","output":"go-commands.json","emits":["deployables"],"interpreter":"python3"},
  {"id":"entrypoints","output":"go-entrypoints.json","emits":["entrypoints"],"interpreter":"python3"}
]
-->

<!-- iterion:script modules -->

```python
import json, os, re, subprocess

ws = os.environ["WORKSPACE_DIR"]
listed = subprocess.run(["git", "-C", ws, "ls-files", "--", "go.mod", "*/go.mod"],
                        capture_output=True, text=True, timeout=120)
mods = sorted(set(listed.stdout.split()))
rows = []
for m in mods:
    text = open(os.path.join(ws, m), encoding="utf-8", errors="replace").read()
    version = re.search(r"^go\s+([0-9][0-9.]*)", text, re.M)
    module = re.search(r"^module\s+(\S+)", text, re.M)
    rows.append({"manifest": m,
                 "module": module.group(1) if module else "",
                 "version": version.group(1) if version else ""})
print(json.dumps({
    "stack": "go",
    "extractor": "modules",
    "facts": {"declared_versions": [
        {"component": "go", "version": r["version"], "evidence": r["manifest"]}
        for r in rows if r["version"]]},
    "modules": rows,
}))
```

<!-- iterion:script commands -->

```python
import json, os, re, subprocess

ws = os.environ["WORKSPACE_DIR"]
listed = subprocess.run(["git", "-C", ws, "ls-files", "--", "*.go"],
                        capture_output=True, text=True, timeout=300)
files = [f for f in listed.stdout.split()
         if not f.startswith("vendor/") and "/vendor/" not in f
         and not f.endswith("_test.go")]
package = re.compile(r"^package\s+main\s*$", re.M)
dirs = set()
for f in files:
    head = open(os.path.join(ws, f), encoding="utf-8", errors="replace").read(8192)
    if package.search(head):
        dirs.add(os.path.dirname(f) or ".")
print(json.dumps({
    "stack": "go",
    "extractor": "commands",
    "facts": {"deployables": len(dirs)},
    "dirs": sorted(dirs),
}))
```

<!-- iterion:script entrypoints -->

```python
import json, os, re, subprocess

ws = os.environ["WORKSPACE_DIR"]
# Route registrations: the standard library's own, and the method-per-verb
# shape every common Go router shares. Matching the SHAPE rather than naming
# routers keeps a new router one line away instead of a release away.
ROUTE = re.compile(
    r"\b(?:http\.HandleFunc|http\.Handle"
    r"|\w+\.(?:GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS"
    r"|Get|Post|Put|Delete|Patch|Handle|HandleFunc|Mount|Route)\s*\()")
listed = subprocess.run(["git", "-C", ws, "ls-files", "--", "*.go"],
                        capture_output=True, text=True, timeout=300)
files = [f for f in listed.stdout.split()
         if not f.startswith("vendor/") and "/vendor/" not in f
         and not f.endswith("_test.go")]
total, hits = 0, []
for f in files:
    n = len(ROUTE.findall(open(os.path.join(ws, f), encoding="utf-8", errors="replace").read()))
    total += n
    if n:
        hits.append({"file": f, "count": n})
print(json.dumps({
    "stack": "go",
    "extractor": "entrypoints",
    "facts": {"entrypoints": total},
    "files": sorted(hits, key=lambda h: h["file"])[:200],
}))
```
