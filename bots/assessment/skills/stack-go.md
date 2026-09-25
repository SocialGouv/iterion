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
environment and runs with the workspace as its working directory. The runner
hands it the git environment every node of the bundle reads under: no ambient
`GIT_DIR`, no global or system configuration, and `core.quotePath=false`, so a
path carrying a non-ASCII byte is listed as itself rather than C-quoted. It reads the
tree through GIT OBJECTS at `BASE_SHA` — `git ls-tree`, `git show <sha>:<path>`
— and never through the checkout. The checkout carries build output, caches and
whatever a previous node left in it; none of that is in the commit the document
says it measured, and an index or a lock file lying there would be counted as
the repository's own.

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

## Manifest shapes

What a Go repository names its build and packaging descriptors. The floor takes
the union of every bundled skill's block, so this one adds Go's names to the
agnostic set without the workflow learning any of them.

<!-- iterion:manifests
["go.mod", "go.work", "go.sum"]
-->

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
sha = os.environ["BASE_SHA"]
# The tree is read through GIT OBJECTS at the pinned commit, never through the
# checkout: the working directory carries build output, caches and whatever a
# previous node left behind, and none of it is in the commit this assessment
# says it measured.
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0", LC_ALL="C", TZ="UTC")

def tree():
    # oid AND path: the bodies are read in BATCHES below, which needs the oid.
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "-l", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    out = {}
    for line in listed.stdout.splitlines():
        head, tab, path = line.partition("\t")
        fields = head.split()
        if tab and path and len(fields) >= 4 and fields[1] == "blob":
            out[path] = (fields[2], int(fields[3]))
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
    ## SIZES COME FROM THE LS-TREE, BEFORE ANY BODY IS READ. Batching by
    ## count alone handed cat-file batches whose bodies -- buffered whole by
    ## one communicate() -- could be gigabytes: BATCH_BYTES was a name, not
    ## a limit, and one oversized blob was buffered whole before its content
    ## was discarded. Blobs over MAX_BLOB are excluded HERE, and the byte
    ## budget closes a batch before it is opened.
    wanted = [(p, oids[p][0], oids[p][1]) for p in paths
              if p in oids and oids[p][1] <= MAX_BLOB]
    batches, batch, budget = [], [], 0
    for entry in wanted:
        if batch and (len(batch) >= BATCH_BLOBS or budget + entry[2] > BATCH_BYTES):
            batches.append(batch)
            batch, budget = [], 0
        batch.append(entry)
        budget += entry[2]
    if batch:
        batches.append(batch)
    for batch in batches:
        proc = subprocess.Popen(["git", "-C", ws, "cat-file", "--batch"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV)
        try:
            body, err = proc.communicate("".join("%s\n" % oid for _, oid, _size in batch).encode("ascii"),
                                         timeout=600)
        except subprocess.TimeoutExpired:
            proc.kill()
            raise SystemExit("reading the blobs of %s timed out" % sha[:12])
        if proc.returncode != 0:
            raise SystemExit("git cat-file --batch exited %d: %s"
                             % (proc.returncode, err.decode("utf-8", "replace")[-300:]))
        cursor = 0
        for path, oid, _size in batch:
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

mods = sorted(p for p in tree() if os.path.basename(p) == "go.mod")
rows = []
for m, text in blobs(mods):
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
sha = os.environ["BASE_SHA"]
# The tree is read through GIT OBJECTS at the pinned commit, never through the
# checkout: the working directory carries build output, caches and whatever a
# previous node left behind, and none of it is in the commit this assessment
# says it measured.
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0", LC_ALL="C", TZ="UTC")

def tree():
    # oid AND path: the bodies are read in BATCHES below, which needs the oid.
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "-l", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    out = {}
    for line in listed.stdout.splitlines():
        head, tab, path = line.partition("\t")
        fields = head.split()
        if tab and path and len(fields) >= 4 and fields[1] == "blob":
            out[path] = (fields[2], int(fields[3]))
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
    ## SIZES COME FROM THE LS-TREE, BEFORE ANY BODY IS READ. Batching by
    ## count alone handed cat-file batches whose bodies -- buffered whole by
    ## one communicate() -- could be gigabytes: BATCH_BYTES was a name, not
    ## a limit, and one oversized blob was buffered whole before its content
    ## was discarded. Blobs over MAX_BLOB are excluded HERE, and the byte
    ## budget closes a batch before it is opened.
    wanted = [(p, oids[p][0], oids[p][1]) for p in paths
              if p in oids and oids[p][1] <= MAX_BLOB]
    batches, batch, budget = [], [], 0
    for entry in wanted:
        if batch and (len(batch) >= BATCH_BLOBS or budget + entry[2] > BATCH_BYTES):
            batches.append(batch)
            batch, budget = [], 0
        batch.append(entry)
        budget += entry[2]
    if batch:
        batches.append(batch)
    for batch in batches:
        proc = subprocess.Popen(["git", "-C", ws, "cat-file", "--batch"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV)
        try:
            body, err = proc.communicate("".join("%s\n" % oid for _, oid, _size in batch).encode("ascii"),
                                         timeout=600)
        except subprocess.TimeoutExpired:
            proc.kill()
            raise SystemExit("reading the blobs of %s timed out" % sha[:12])
        if proc.returncode != 0:
            raise SystemExit("git cat-file --batch exited %d: %s"
                             % (proc.returncode, err.decode("utf-8", "replace")[-300:]))
        cursor = 0
        for path, oid, _size in batch:
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

files = [f for f in tree() if f.endswith(".go")
         and not f.startswith("vendor/") and "/vendor/" not in f
         and not f.endswith("_test.go")]
package = re.compile(r"^package\s+main\s*$", re.M)
dirs = set()
for f, text in blobs(files):
    if package.search(text[:8192]):
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
sha = os.environ["BASE_SHA"]
# The tree is read through GIT OBJECTS at the pinned commit, never through the
# checkout: the working directory carries build output, caches and whatever a
# previous node left behind, and none of it is in the commit this assessment
# says it measured.
ENV = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0", LC_ALL="C", TZ="UTC")

def tree():
    # oid AND path: the bodies are read in BATCHES below, which needs the oid.
    listed = subprocess.run(["git", "-C", ws, "ls-tree", "-r", "-l", "--full-tree", sha],
                            capture_output=True, text=True, env=ENV, timeout=300)
    if listed.returncode != 0:
        raise SystemExit("cannot list the tree at %s: %s" % (sha[:12], listed.stderr.strip()[-300:]))
    out = {}
    for line in listed.stdout.splitlines():
        head, tab, path = line.partition("\t")
        fields = head.split()
        if tab and path and len(fields) >= 4 and fields[1] == "blob":
            out[path] = (fields[2], int(fields[3]))
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
    ## SIZES COME FROM THE LS-TREE, BEFORE ANY BODY IS READ. Batching by
    ## count alone handed cat-file batches whose bodies -- buffered whole by
    ## one communicate() -- could be gigabytes: BATCH_BYTES was a name, not
    ## a limit, and one oversized blob was buffered whole before its content
    ## was discarded. Blobs over MAX_BLOB are excluded HERE, and the byte
    ## budget closes a batch before it is opened.
    wanted = [(p, oids[p][0], oids[p][1]) for p in paths
              if p in oids and oids[p][1] <= MAX_BLOB]
    batches, batch, budget = [], [], 0
    for entry in wanted:
        if batch and (len(batch) >= BATCH_BLOBS or budget + entry[2] > BATCH_BYTES):
            batches.append(batch)
            batch, budget = [], 0
        batch.append(entry)
        budget += entry[2]
    if batch:
        batches.append(batch)
    for batch in batches:
        proc = subprocess.Popen(["git", "-C", ws, "cat-file", "--batch"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=ENV)
        try:
            body, err = proc.communicate("".join("%s\n" % oid for _, oid, _size in batch).encode("ascii"),
                                         timeout=600)
        except subprocess.TimeoutExpired:
            proc.kill()
            raise SystemExit("reading the blobs of %s timed out" % sha[:12])
        if proc.returncode != 0:
            raise SystemExit("git cat-file --batch exited %d: %s"
                             % (proc.returncode, err.decode("utf-8", "replace")[-300:]))
        cursor = 0
        for path, oid, _size in batch:
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

# Route registrations: the standard library's own, and the method-per-verb
# shape every common Go router shares. Matching the SHAPE rather than naming
# routers keeps a new router one line away instead of a release away -- but
# the same shape is a CLIENT call on the objects that consume routes:
# http.Get(, resp.Header.Get( and their kin were counted as route
# registrations, and the measured total answered a question nobody asked.
# Receivers that name readers and transports are excluded; a router named
# like a client is still missed, and the declared route count is the
# figure the letter publishes.
ROUTE = re.compile(
    r"\b(?:http\.HandleFunc|http\.Handle"
    r"|(?!(?:[Hh]ttp|[Hh]eader|resp|response|[Rr]eq|request|client|ctx|context"
    r"|conn|rows?|vals|values|params|form|session|cache)\b)"
    r"\w+\.(?:GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS"
    r"|Get|Post|Put|Delete|Patch|Handle|HandleFunc|Mount|Route)\s*\()")
files = [f for f in tree() if f.endswith(".go")
         and not f.startswith("vendor/") and "/vendor/" not in f
         and not f.endswith("_test.go")]
total, hits = 0, []
for f, text in blobs(files):
    n = len(ROUTE.findall(text))
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
