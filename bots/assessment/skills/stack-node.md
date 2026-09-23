---
name: stack-node
description: Node.js extractors for the assessment — the declared runtime and package-manager versions, the runnable packages, and the HTTP entrypoints, each emitted as JSON by a deterministic script carried in this file. Loaded when the survey names the `node` stack.
---

# stack-node — what an assessment reads out of a Node.js tree

Detection signals: a `package.json` at the workspace root or in a workspace
member, and a lock file beside it.

The workflow runs the extractors declared at the bottom of this file. Each one
writes one JSON document to standard output, which the runner captures under
`$SCRATCH_DIR`; the coverage gate then verifies the artefact rather than the
exit code, because a script that exits 0 and writes nothing is a silent
coverage gap.

Every script receives `WORKSPACE_DIR`, `SCRATCH_DIR` and `BASE_SHA` in its
environment and runs with the workspace as its working directory.

## What each extractor is responsible for

- **`packages`** — the declared runtime and package-manager versions
  (`engines`, `packageManager`, `volta`) across every `package.json` that is
  not inside `node_modules`. Read from the declaration: what the repository
  ASKS for is the fact an assessment reports, and the installed toolchain of
  whoever ran it is not.
- **`runnables`** — the packages that are deployed or run on their own, taken
  as the ones declaring a `bin` or a `start` script. It over-counts a
  repository whose tooling declares `start` too; the survey's `deployable`
  declarations are the correction.
- **`entrypoints`** — the HTTP surface, counted from the route registrations
  the common server frameworks share, over the repository's own sources.
  Declaration files, minified bundles and installed dependencies are excluded.

## Reading the result

A monorepo yields several packages, and that is not double counting: the
assessment sums first-party lines over the declared perimeter, not per
package. What does distort the count is a `package.json` inside a build output
committed to the tree — declare that subtree `excluded` in the survey and the
lint will hold the perimeter to it.

Adding a framework to the pattern set is an edit HERE, never to the workflow.

## Extractors

<!-- iterion:extractors
[
  {"id":"packages","output":"node-packages.json","emits":["declared_versions"],"interpreter":"python3"},
  {"id":"runnables","output":"node-runnables.json","emits":["deployables"],"interpreter":"python3"},
  {"id":"entrypoints","output":"node-entrypoints.json","emits":["entrypoints"],"interpreter":"python3"}
]
-->

<!-- iterion:script packages -->

```python
import json, os, subprocess

ws = os.environ["WORKSPACE_DIR"]
listed = subprocess.run(["git", "-C", ws, "ls-files", "--", "package.json", "*/package.json"],
                        capture_output=True, text=True, timeout=120)
manifests = [m for m in sorted(set(listed.stdout.split())) if "node_modules/" not in m]
versions, rows = [], []
for m in manifests:
    try:
        data = json.load(open(os.path.join(ws, m), encoding="utf-8"))
    except (ValueError, OSError) as exc:
        rows.append({"manifest": m, "unreadable": str(exc)})
        continue
    engines = data.get("engines") or {}
    declared = {}
    if isinstance(engines, dict):
        for component, spec in engines.items():
            if isinstance(spec, str):
                declared[component] = spec
    if isinstance(data.get("packageManager"), str):
        declared["packageManager"] = data["packageManager"]
    volta = data.get("volta")
    if isinstance(volta, dict):
        for component, spec in volta.items():
            if isinstance(spec, str):
                declared["volta." + component] = spec
    rows.append({"manifest": m, "name": data.get("name") or "", "declared": declared})
    for component, spec in declared.items():
        versions.append({"component": component, "version": spec, "evidence": m})
print(json.dumps({
    "stack": "node",
    "extractor": "packages",
    "facts": {"declared_versions": versions},
    "packages": rows,
}))
```

<!-- iterion:script runnables -->

```python
import json, os, subprocess

ws = os.environ["WORKSPACE_DIR"]
listed = subprocess.run(["git", "-C", ws, "ls-files", "--", "package.json", "*/package.json"],
                        capture_output=True, text=True, timeout=120)
manifests = [m for m in sorted(set(listed.stdout.split())) if "node_modules/" not in m]
runnable = []
for m in manifests:
    try:
        data = json.load(open(os.path.join(ws, m), encoding="utf-8"))
    except (ValueError, OSError):
        continue
    scripts = data.get("scripts") or {}
    has_start = isinstance(scripts, dict) and isinstance(scripts.get("start"), str)
    has_bin = bool(data.get("bin"))
    if has_start or has_bin:
        runnable.append({"manifest": m, "name": data.get("name") or "",
                         "start": has_start, "bin": has_bin})
print(json.dumps({
    "stack": "node",
    "extractor": "runnables",
    "facts": {"deployables": len(runnable)},
    "runnables": runnable,
}))
```

<!-- iterion:script entrypoints -->

```python
import json, os, re, subprocess

ws = os.environ["WORKSPACE_DIR"]
# The method-per-verb shape every common Node server framework shares, plus
# the router-mount form. Matching the SHAPE rather than naming frameworks
# keeps a new one an edit to this pattern instead of a release.
ROUTE = re.compile(
    r"\b\w+\.(?:get|post|put|delete|patch|head|options|all|use|route|register)\s*\(\s*"
    r"(?:`|'|\")/")
listed = subprocess.run(
    ["git", "-C", ws, "ls-files", "--", "*.js", "*.mjs", "*.cjs", "*.ts", "*.tsx", "*.jsx"],
    capture_output=True, text=True, timeout=300)
files = [f for f in listed.stdout.split()
         if "node_modules/" not in f and not f.endswith(".d.ts")
         and ".min." not in os.path.basename(f)]
total, hits = 0, []
for f in files:
    n = len(ROUTE.findall(open(os.path.join(ws, f), encoding="utf-8", errors="replace").read()))
    total += n
    if n:
        hits.append({"file": f, "count": n})
print(json.dumps({
    "stack": "node",
    "extractor": "entrypoints",
    "facts": {"entrypoints": total},
    "files": sorted(hits, key=lambda h: h["file"])[:200],
}))
```
