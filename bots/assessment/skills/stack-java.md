---
name: stack-java
description: Java extractors for the assessment — the declared JVM and framework versions, the modules the build ships on their own, and the HTTP entrypoints, each emitted as JSON by a deterministic script carried in this file. Loaded when the survey names the `java` stack.
---

# stack-java — what an assessment reads out of a Java tree

Detection signals: a `build.gradle`, `build.gradle.kts`, `pom.xml` or
`gradle/wrapper/gradle-wrapper.properties` at the workspace root or in a
module directory, and `.java` sources beside them.

The workflow runs the extractors declared at the bottom of this file. Each one
writes one JSON document to standard output, which the runner captures under
`$SCRATCH_DIR`; the coverage gate then verifies the artefact rather than the
exit code, because a script that exits 0 and writes nothing is a silent
coverage gap.

Every script receives `WORKSPACE_DIR`, `SCRATCH_DIR` and `BASE_SHA` in its
environment and runs with the workspace as its working directory. The runner
hands it the git environment every node of the bundle reads under: no ambient
`GIT_DIR`, no global or system configuration, and `core.quotePath=false`. It reads
the tree through GIT OBJECTS at `BASE_SHA` and never through the checkout,
where a `build/` or `target/` output would be counted as the repository's own.

## What each extractor is responsible for

- **`packages`** — the declared JVM and framework versions across the build
  descriptors: the Gradle wrapper, `sourceCompatibility` / toolchain /
  `options.release`, the Maven `java.version` and compiler target, the
  Spring Boot version (Gradle plugin or starter parent), and `.sdkmanrc`.
  Read from the declaration: what the build ASKS for is the fact an
  assessment reports, and the JDK of whoever ran it is not.
- **`runnables`** — the modules the build ships on their own, taken as the
  ones assembling a `@SpringBootApplication` class or declaring a Spring
  Boot plugin (Gradle or Maven). It over-counts a repository whose tooling
  module carries the plugin too; the survey's `deployable` declarations are
  the correction.
- **`entrypoints`** — the HTTP surface, counted from the mapping
  annotations Spring MVC and JAX-RS share, over the repository's own
  sources. Build outputs (`build/`, `target/`, `.gradle/`) are excluded.

A class-level `@RequestMapping` is one registration here, and the prefix of
the methods below it: the extractor counts declared SITES, not resolved
URLs, exactly as the Node extractor counts route registrations. What a
finer reading asserts, the survey's declarations assert.

Adding a framework to the annotation pattern is an edit HERE, never to the
workflow.

## Manifest shapes

What a Java repository names its build and packaging descriptors. The floor
takes the union of every bundled skill's block, so this one adds Java's
names to the agnostic set without the workflow learning any of them.

<!-- iterion:manifests
["build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts",
 "gradle.properties", "gradle/wrapper/gradle-wrapper.properties", "pom.xml",
 ".sdkmanrc"]
-->

## Extractors

<!-- iterion:extractors
[
  {"id":"packages","output":"java-versions.json","emits":["declared_versions"],"interpreter":"python3"},
  {"id":"runnables","output":"java-runnables.json","emits":["deployables"],"interpreter":"python3"},
  {"id":"entrypoints","output":"java-entrypoints.json","emits":["entrypoints"],"interpreter":"python3"}
]
-->

<!-- iterion:script packages -->

```python
import json, os, re, subprocess

ws = os.environ["WORKSPACE_DIR"]
sha = os.environ["BASE_SHA"]
# The tree is read through GIT OBJECTS at the pinned commit, never through the
# checkout: a build output (build/, target/) or an IDE cache lying in the
# working directory is in no commit, and counting it would make the repository
# look like whatever was last compiled in it.
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
# -- past the runner's wall, after which no output lands and the measurement
# publishes a zero it never took. Bounded by a blob count and a byte budget,
# so a tree of few huge files is bounded too. The floor reads the same way.
BATCH_BLOBS, BATCH_BYTES, MAX_BLOB = 512, 64 * 1024 * 1024, 2 * 1024 * 1024

def blobs(paths):
    """Yield (path, text) for each path, reading the bodies in batches."""
    oids = tree()
    ## SIZES COME FROM THE LS-TREE, BEFORE ANY BODY IS READ. Batching by
    ## count alone handed cat-file batches whose bodies -- buffered whole by
    ## one communicate() -- could be gigabytes. Blobs over MAX_BLOB are
    ## excluded HERE, and the byte budget closes a batch before it opens.
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

def is_output(path):
    # A Gradle or Maven build output, or an IDE working state: compiled
    # classes and generated sources are no one's declared surface.
    parts = path.split("/")
    return "build/" in path or path.startswith("build/") or "target/" in path \
        or path.startswith("target/") or ".gradle/" in path

GRADLE_FILES = ("build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts")
POM = "pom.xml"

def read_manifests():
    paths = [p for p in tree()
             if (os.path.basename(p) in GRADLE_FILES or os.path.basename(p) == POM
                 or os.path.basename(p) == ".sdkmanrc"
                 or p.endswith("gradle/wrapper/gradle-wrapper.properties"))
             and not is_output(p)]
    return paths

GRADLE_JAVA = [
    # The declared Java target, in the shapes Gradle has used across its
    # majors: the property, the toolchain language version, and the release
    # flag. What the build ASKS for is the fact; the JDK of whoever ran it
    # is not.
    ("java.sourceCompatibility", re.compile(r"sourceCompatibility\s*=\s*['\"]?([0-9][0-9._]*)")),
    ("java.toolchain", re.compile(r"JavaLanguageVersion\.of\(\s*([0-9]+)\s*\)")),
    ("java.release", re.compile(r"options\.release\.(?:set\()?\s*([0-9]+)")),
    ("java.release", re.compile(r"options\.release\s*=\s*(['\"]?)([0-9]+)\1")),
]
GRADLE_BOOT = re.compile(r"id\s*\(?[\s]*['\"]org\.springframework\.boot['\"]\)?[\s]*(?:version[\s]*)?['\"]([^'\"]+)['\"]")
WRAPPER = re.compile(r"gradle-([0-9][0-9.]*)-(?:bin|all)\.zip")
SDKMAN = re.compile(r"^java\s*=\s*(\S+)", re.M)
POM_JAVA = [
    ("java.version", re.compile(r"<java\.version>\s*([^<\s]+)\s*</java\.version>")),
    ("java.release", re.compile(r"<maven\.compiler\.release>\s*([^<\s]+)\s*</maven\.compiler\.release>")),
    ("java.target", re.compile(r"<maven\.compiler\.target>\s*([^<\s]+)\s*</maven\.compiler\.target>")),
    ("java.target", re.compile(r"<target>\s*([0-9][0-9._]*)\s*</target>")),
]

rows, versions = [], []
for m in read_manifests():
    text = blob(m)
    declared = {}
    base = os.path.basename(m)
    if base == "gradle-wrapper.properties":
        match = WRAPPER.search(text)
        if match:
            declared["gradle.wrapper"] = match.group(1)
    elif base == ".sdkmanrc":
        match = SDKMAN.search(text)
        if match:
            declared["java.sdkman"] = match.group(1)
    elif base == POM:
        for component, pattern in POM_JAVA:
            match = pattern.search(text)
            if match:
                declared[component] = match.group(1)
        # The Spring Boot version a Maven build declares is the PARENT's
        # version -- one line above its artifactId, inside <parent>.
        parent = re.search(r"<parent>.*?</parent>", text, re.S)
        if parent and "spring-boot-starter-parent" in parent.group(0):
            match = re.search(r"<version>\s*([^<\s]+)\s*</version>", parent.group(0))
            if match:
                declared["spring-boot"] = match.group(1)
    else:
        for component, pattern in GRADLE_JAVA:
            match = pattern.search(text)
            if match:
                declared[component] = match.group(match.lastindex)
        match = GRADLE_BOOT.search(text)
        if match:
            declared["spring-boot"] = match.group(1)
    if not declared:
        continue
    rows.append({"manifest": m, "name": os.path.basename(os.path.dirname(m)) or ".", "declared": declared})
    for component, spec in declared.items():
        versions.append({"component": component, "version": spec, "evidence": m})
print(json.dumps({
    "stack": "java",
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
# checkout: a build output (build/, target/) or an IDE cache lying in the
# working directory is in no commit, and counting it would make the repository
# look like whatever was last compiled in it.
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
# -- past the runner's wall, after which no output lands and the measurement
# publishes a zero it never took. Bounded by a blob count and a byte budget,
# so a tree of few huge files is bounded too. The floor reads the same way.
BATCH_BLOBS, BATCH_BYTES, MAX_BLOB = 512, 64 * 1024 * 1024, 2 * 1024 * 1024

def blobs(paths):
    """Yield (path, text) for each path, reading the bodies in batches."""
    oids = tree()
    ## SIZES COME FROM THE LS-TREE, BEFORE ANY BODY IS READ. Batching by
    ## count alone handed cat-file batches whose bodies -- buffered whole by
    ## one communicate() -- could be gigabytes. Blobs over MAX_BLOB are
    ## excluded HERE, and the byte budget closes a batch before it opens.
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

def is_output(path):
    # A Gradle or Maven build output, or an IDE working state: compiled
    # classes and generated sources are no one's declared surface.
    parts = path.split("/")
    return "build/" in path or path.startswith("build/") or "target/" in path \
        or path.startswith("target/") or ".gradle/" in path

SOURCE = (".java",)

def is_output(path):
    parts = path.split("/")
    return "build/" in path or path.startswith("build/") or "target/" in path \
        or path.startswith("target/") or ".gradle/" in path

# A deployable is an application the build ships on its own. The Boot
# annotation names the class that ASSEMBLES one; the Boot plugins in the
# build scripts name the module that PACKAGES one. Either is a deployable
# the tree declares; the survey's deployable declarations are the
# correction for tooling that over-counts.
BOOT_APP = re.compile(r"@SpringBootApplication\b")
BOOT_PLUGIN_GRADLE = re.compile(r"org\.springframework\.boot.*\b(?:springBoot|bootJar|org\.springframework\.boot\.gradle\.plugin)\b|id[\s]*['\"]org\.springframework\.boot['\"]")
BOOT_PLUGIN_POM = re.compile(r"spring-boot-maven-plugin")

def module_name(path, settings_names):
    # The module a source file or build script belongs to: the nearest
    # settings file at or above it, named by rootProject.name. The walk
    # tests the ROOT too -- dirname of a root-level script is '' and a
    # `while d` loop stops one step short of it, splitting one module in
    # two under two names.
    d = os.path.dirname(path)
    while True:
        if d in settings_names:
            return settings_names[d]
        parent = os.path.dirname(d)
        if parent == d:
            break
        d = parent
    return d or "."

files = [f for f in tree() if f.endswith(SOURCE) and not is_output(f)]
apps = []
seen_files = set()
for f, text in blobs(files):
    if BOOT_APP.search(text):
        seen_files.add(f)
settings = {}
for f in tree():
    base = os.path.basename(f)
    if base in ("settings.gradle", "settings.gradle.kts") and not is_output(f):
        text = blob(f)
        match = re.search(r"rootProject\.name\s*=\s*['\"]([^'\"]+)['\"]", text)
        settings[os.path.dirname(f)] = match.group(1) if match else "."
builds = []
for f in tree():
    base = os.path.basename(f)
    if base in ("build.gradle", "build.gradle.kts") and not is_output(f):
        builds.append(f)
    elif base == "pom.xml" and not is_output(f):
        builds.append(f)
boot_builds = set()
for f, text in blobs(builds):
    if BOOT_PLUGIN_GRADLE.search(text) or BOOT_PLUGIN_POM.search(text):
        boot_builds.add(f)
# ONE identity per module: app classes and build scripts resolve through
# the SAME settings walk, so a root application would otherwise be
# counted twice -- once by its class, once by its build script, under
# two names -- and the measurement would publish two deployables for
# one artefact.
modules = set()
for f in sorted(seen_files | boot_builds):
    modules.add(module_name(f, settings))
runnable = [{"module": m, "boot": True} for m in sorted(modules)]
print(json.dumps({
    "stack": "java",
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
# checkout: a build output (build/, target/) or an IDE cache lying in the
# working directory is in no commit, and counting it would make the repository
# look like whatever was last compiled in it.
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
# -- past the runner's wall, after which no output lands and the measurement
# publishes a zero it never took. Bounded by a blob count and a byte budget,
# so a tree of few huge files is bounded too. The floor reads the same way.
BATCH_BLOBS, BATCH_BYTES, MAX_BLOB = 512, 64 * 1024 * 1024, 2 * 1024 * 1024

def blobs(paths):
    """Yield (path, text) for each path, reading the bodies in batches."""
    oids = tree()
    ## SIZES COME FROM THE LS-TREE, BEFORE ANY BODY IS READ. Batching by
    ## count alone handed cat-file batches whose bodies -- buffered whole by
    ## one communicate() -- could be gigabytes. Blobs over MAX_BLOB are
    ## excluded HERE, and the byte budget closes a batch before it opens.
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

def is_output(path):
    # A Gradle or Maven build output, or an IDE working state: compiled
    # classes and generated sources are no one's declared surface.
    parts = path.split("/")
    return "build/" in path or path.startswith("build/") or "target/" in path \
        or path.startswith("target/") or ".gradle/" in path

# The method-per-annotation shape Spring MVC and JAX-RS share, counted the
# way the node extractor counts route registrations: an annotation SITE, not
# a resolved URL. A class-level @RequestMapping is one registration here and
# the prefix of the methods below it -- the assessment reports the count of
# declared sites, and the survey's declarations are where a finer reading is
# asserted. Matching the SHAPE rather than naming frameworks keeps a new
# one an edit to this pattern instead of a release.
ROUTE = re.compile(r"@(?:Get|Post|Put|Delete|Patch|Request)Mapping\b|@(?:GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\b")
SOURCE = (".java",)

def is_output(path):
    parts = path.split("/")
    return "build/" in path or path.startswith("build/") or "target/" in path \
        or path.startswith("target/") or ".gradle/" in path

files = [f for f in tree() if f.endswith(SOURCE) and not is_output(f)]
total, hits = 0, []
for f, text in blobs(files):
    n = len(ROUTE.findall(text))
    total += n
    if n:
        hits.append({"file": f, "count": n})
print(json.dumps({
    "stack": "java",
    "extractor": "entrypoints",
    "facts": {"entrypoints": total},
    "files": sorted(hits, key=lambda h: h["file"])[:200],
}))
```
