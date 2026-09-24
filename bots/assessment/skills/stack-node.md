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
environment and runs with the workspace as its working directory. It reads the
tree through GIT OBJECTS at `BASE_SHA` — `git ls-tree`, `git show <sha>:<path>`
— and never through the checkout, where an installed `node_modules` or a build
output would be counted as the repository's own.

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
import json, os, re, subprocess

ws = os.environ["WORKSPACE_DIR"]
sha = os.environ["BASE_SHA"]
# The tree is read through GIT OBJECTS at the pinned commit, never through the
# checkout: an installed `node_modules`, a build output or a lock file lying in
# the working directory is in no commit, and counting it would make the
# repository look like whatever was last run in it.
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0", LC_ALL="C", TZ="UTC")

def tree():
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "--name-only", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    return [p for p in listed.stdout.splitlines() if p]

def blob(path):
    shown = subprocess.run(["git", "-C", ws, "show", "%s:%s" % (sha, path)],
                           capture_output=True, env=ENV, timeout=120)
    return shown.stdout.decode("utf-8", "replace") if shown.returncode == 0 else ""

def manifests():
    return [p for p in tree() if os.path.basename(p) == "package.json" and "node_modules/" not in p]

versions, rows = [], []
for m in sorted(manifests()):
    try:
        data = json.loads(blob(m))
    except ValueError as exc:
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
import json, os, re, subprocess

ws = os.environ["WORKSPACE_DIR"]
sha = os.environ["BASE_SHA"]
# The tree is read through GIT OBJECTS at the pinned commit, never through the
# checkout: an installed `node_modules`, a build output or a lock file lying in
# the working directory is in no commit, and counting it would make the
# repository look like whatever was last run in it.
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0", LC_ALL="C", TZ="UTC")

def tree():
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "--name-only", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    return [p for p in listed.stdout.splitlines() if p]

def blob(path):
    shown = subprocess.run(["git", "-C", ws, "show", "%s:%s" % (sha, path)],
                           capture_output=True, env=ENV, timeout=120)
    return shown.stdout.decode("utf-8", "replace") if shown.returncode == 0 else ""

def manifests():
    return [p for p in tree() if os.path.basename(p) == "package.json" and "node_modules/" not in p]

runnable = []
for m in sorted(manifests()):
    try:
        data = json.loads(blob(m))
    except ValueError:
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
sha = os.environ["BASE_SHA"]
# The tree is read through GIT OBJECTS at the pinned commit, never through the
# checkout: an installed `node_modules`, a build output or a lock file lying in
# the working directory is in no commit, and counting it would make the
# repository look like whatever was last run in it.
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0", LC_ALL="C", TZ="UTC")

def tree():
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "--name-only", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    return [p for p in listed.stdout.splitlines() if p]

def blob(path):
    shown = subprocess.run(["git", "-C", ws, "show", "%s:%s" % (sha, path)],
                           capture_output=True, env=ENV, timeout=120)
    return shown.stdout.decode("utf-8", "replace") if shown.returncode == 0 else ""

def manifests():
    return [p for p in tree() if os.path.basename(p) == "package.json" and "node_modules/" not in p]

# The method-per-verb shape every common Node server framework shares, plus
# the router-mount form. Matching the SHAPE rather than naming frameworks
# keeps a new one an edit to this pattern instead of a release.
ROUTE = re.compile(
    r"\b\w+\.(?:get|post|put|delete|patch|head|options|all|use|route|register)\s*\(\s*"
    r"(?:`|'|\")/")
SOURCE = (".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx")
files = [f for f in tree() if f.endswith(SOURCE)
         and "node_modules/" not in f and not f.endswith(".d.ts")
         and ".min." not in os.path.basename(f)]
total, hits = 0, []
for f in files:
    n = len(ROUTE.findall(blob(f)))
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
