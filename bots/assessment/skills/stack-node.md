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
environment and runs with the workspace as its working directory. The runner
hands it the git environment every node of the bundle reads under: no ambient
`GIT_DIR`, no global or system configuration, and `core.quotePath=false`, so a
path carrying a non-ASCII byte is listed as itself rather than C-quoted. It reads the
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

## Manifest shapes

What a Node.js repository names its build and packaging descriptors. The floor
takes the union of every bundled skill's block, so this one adds Node's names
to the agnostic set without the workflow learning any of them.

<!-- iterion:manifests
["package.json", "pnpm-workspace.yaml", "lerna.json", "turbo.json", "nx.json",
 "tsconfig.json", "deno.json", "deno.jsonc"]
-->

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
    # oid AND path: the bodies are read in BATCHES below, which needs the oid.
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    out = {}
    for line in listed.stdout.splitlines():
        head, tab, path = line.partition("\t")
        fields = head.split()
        if tab and path and len(fields) >= 3 and fields[1] == "blob":
            out[path] = fields[2]
    return out

# ONE PROCESS PER BATCH, never one per file. A `git show` per path is ~5-10 ms
# of fork, which is minutes on the large legacy tree this assessment exists for
# — past the runner's wall, after which no output lands and the measurement
# publishes a zero it never took. Bounded by a blob count and a byte budget, so
# a tree of few huge files is bounded too. The floor reads the same way.
BATCH_BLOBS, BATCH_BYTES, MAX_BLOB = 512, 64 * 1024 * 1024, 2 * 1024 * 1024

def blobs(paths):
    """Yield (path, text) for each path, reading the bodies in batches."""
    oids = tree()
    wanted = [(p, oids[p]) for p in paths if p in oids]
    for start in range(0, len(wanted), BATCH_BLOBS):
        batch, budget = [], 0
        for entry in wanted[start:start + BATCH_BLOBS]:
            batch.append(entry)
            budget += 1
        proc = subprocess.Popen(["git", "-C", ws, "cat-file", "--batch"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV)
        try:
            body, err = proc.communicate("".join("%s\n" % oid for _, oid in batch).encode("ascii"),
                                         timeout=600)
        except subprocess.TimeoutExpired:
            proc.kill()
            raise SystemExit("reading the blobs of %s timed out" % sha[:12])
        if proc.returncode != 0:
            raise SystemExit("git cat-file --batch exited %d: %s"
                             % (proc.returncode, err.decode("utf-8", "replace")[-300:]))
        cursor = 0
        for path, oid in batch:
            newline = body.find(b"\n", cursor)
            if newline < 0:
                raise SystemExit("git cat-file --batch output ended inside a header")
            header = body[cursor:newline].split()
            if len(header) != 3:
                raise SystemExit("git cat-file --batch refused %s: %s"
                                 % (oid[:12], body[cursor:newline].decode("utf-8", "replace")))
            length = int(header[2])
            raw = body[newline + 1:newline + 1 + length]
            cursor = newline + 1 + length + 1
            yield path, ("" if length > MAX_BLOB else raw.decode("utf-8", "replace"))

def blob(path):
    for _, text in blobs([path]):
        return text
    return ""

def manifests():
    return [p for p in tree() if os.path.basename(p) == "package.json" and "node_modules/" not in p]

versions, rows = [], []
for m, text in blobs(sorted(manifests())):
    try:
        data = json.loads(text)
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
    # oid AND path: the bodies are read in BATCHES below, which needs the oid.
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    out = {}
    for line in listed.stdout.splitlines():
        head, tab, path = line.partition("\t")
        fields = head.split()
        if tab and path and len(fields) >= 3 and fields[1] == "blob":
            out[path] = fields[2]
    return out

# ONE PROCESS PER BATCH, never one per file. A `git show` per path is ~5-10 ms
# of fork, which is minutes on the large legacy tree this assessment exists for
# — past the runner's wall, after which no output lands and the measurement
# publishes a zero it never took. Bounded by a blob count and a byte budget, so
# a tree of few huge files is bounded too. The floor reads the same way.
BATCH_BLOBS, BATCH_BYTES, MAX_BLOB = 512, 64 * 1024 * 1024, 2 * 1024 * 1024

def blobs(paths):
    """Yield (path, text) for each path, reading the bodies in batches."""
    oids = tree()
    wanted = [(p, oids[p]) for p in paths if p in oids]
    for start in range(0, len(wanted), BATCH_BLOBS):
        batch, budget = [], 0
        for entry in wanted[start:start + BATCH_BLOBS]:
            batch.append(entry)
            budget += 1
        proc = subprocess.Popen(["git", "-C", ws, "cat-file", "--batch"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV)
        try:
            body, err = proc.communicate("".join("%s\n" % oid for _, oid in batch).encode("ascii"),
                                         timeout=600)
        except subprocess.TimeoutExpired:
            proc.kill()
            raise SystemExit("reading the blobs of %s timed out" % sha[:12])
        if proc.returncode != 0:
            raise SystemExit("git cat-file --batch exited %d: %s"
                             % (proc.returncode, err.decode("utf-8", "replace")[-300:]))
        cursor = 0
        for path, oid in batch:
            newline = body.find(b"\n", cursor)
            if newline < 0:
                raise SystemExit("git cat-file --batch output ended inside a header")
            header = body[cursor:newline].split()
            if len(header) != 3:
                raise SystemExit("git cat-file --batch refused %s: %s"
                                 % (oid[:12], body[cursor:newline].decode("utf-8", "replace")))
            length = int(header[2])
            raw = body[newline + 1:newline + 1 + length]
            cursor = newline + 1 + length + 1
            yield path, ("" if length > MAX_BLOB else raw.decode("utf-8", "replace"))

def blob(path):
    for _, text in blobs([path]):
        return text
    return ""

def manifests():
    return [p for p in tree() if os.path.basename(p) == "package.json" and "node_modules/" not in p]

runnable = []
for m, text in blobs(sorted(manifests())):
    try:
        data = json.loads(text)
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
    # oid AND path: the bodies are read in BATCHES below, which needs the oid.
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    out = {}
    for line in listed.stdout.splitlines():
        head, tab, path = line.partition("\t")
        fields = head.split()
        if tab and path and len(fields) >= 3 and fields[1] == "blob":
            out[path] = fields[2]
    return out

# ONE PROCESS PER BATCH, never one per file. A `git show` per path is ~5-10 ms
# of fork, which is minutes on the large legacy tree this assessment exists for
# — past the runner's wall, after which no output lands and the measurement
# publishes a zero it never took. Bounded by a blob count and a byte budget, so
# a tree of few huge files is bounded too. The floor reads the same way.
BATCH_BLOBS, BATCH_BYTES, MAX_BLOB = 512, 64 * 1024 * 1024, 2 * 1024 * 1024

def blobs(paths):
    """Yield (path, text) for each path, reading the bodies in batches."""
    oids = tree()
    wanted = [(p, oids[p]) for p in paths if p in oids]
    for start in range(0, len(wanted), BATCH_BLOBS):
        batch, budget = [], 0
        for entry in wanted[start:start + BATCH_BLOBS]:
            batch.append(entry)
            budget += 1
        proc = subprocess.Popen(["git", "-C", ws, "cat-file", "--batch"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV)
        try:
            body, err = proc.communicate("".join("%s\n" % oid for _, oid in batch).encode("ascii"),
                                         timeout=600)
        except subprocess.TimeoutExpired:
            proc.kill()
            raise SystemExit("reading the blobs of %s timed out" % sha[:12])
        if proc.returncode != 0:
            raise SystemExit("git cat-file --batch exited %d: %s"
                             % (proc.returncode, err.decode("utf-8", "replace")[-300:]))
        cursor = 0
        for path, oid in batch:
            newline = body.find(b"\n", cursor)
            if newline < 0:
                raise SystemExit("git cat-file --batch output ended inside a header")
            header = body[cursor:newline].split()
            if len(header) != 3:
                raise SystemExit("git cat-file --batch refused %s: %s"
                                 % (oid[:12], body[cursor:newline].decode("utf-8", "replace")))
            length = int(header[2])
            raw = body[newline + 1:newline + 1 + length]
            cursor = newline + 1 + length + 1
            yield path, ("" if length > MAX_BLOB else raw.decode("utf-8", "replace"))

def blob(path):
    for _, text in blobs([path]):
        return text
    return ""

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
for f, text in blobs(files):
    n = len(ROUTE.findall(text))
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
