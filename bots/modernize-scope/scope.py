#!/usr/bin/env python3
"""The deterministic core of a framing pass: the perimeter, and the floor.

Three tool nodes run this one body. `scope_write` turns the surveyor's
declarations into a canonical perimeter and writes an audit copy of it;
`scope_lint` checks that perimeter against the tree; `floor` measures what the
perimeter leaves. The layers are ordered so that deleting one degrades the
DIAGNOSIS and never the guarantee: the floor re-runs the perimeter guards it
depends on, whether or not the linter ran.

Everything here reads GIT OBJECTS at a pinned SHA. Nothing is detected, guessed
or defaulted: the perimeter comes from the declared scope, and a declaration
whose evidence is absent is refused rather than skipped.

Not one language, package manager or ecosystem is named in this file. Extensions
are DISCOVERED from the tree and reported as they are found; the file has no
opinion about which of them matter. Anything stack-specific belongs to a
`lang-<id>` skill and to the extractor it prescribes — not here.

Refusals leave by the same door as results: a JSON envelope on stdout and exit
0, so the workflow routes them to a typed fail node instead of the runtime
turning a non-zero exit into an opaque tool error.
"""

import json
import os
import re
import subprocess
import sys

# ---- shared body below (kept byte-identical with the inlined copy) ----

# git reads the ambient configuration and the ambient configuration changes its
# OUTPUT: with `log.showSignature=true` a gpg banner lands inside a commit date
# and every dated figure moves. One environment for every call, at the choke
# point. `GIT_NO_REPLACE_OBJECTS` neutralises replace refs, which silently moved
# a commit count by 2 on a real repository.
GIT_SCRUB = {
    "GIT_CONFIG_GLOBAL": "/dev/null",
    "GIT_CONFIG_SYSTEM": "/dev/null",
    "GIT_CONFIG_NOSYSTEM": "1",
    "GIT_TERMINAL_PROMPT": "0",
    "GIT_NO_REPLACE_OBJECTS": "1",
    "LC_ALL": "C",
    "TZ": "UTC",
    # A writing git command detaches `git maintenance run --auto`, which is
    # still writing under .git/objects after the command returns — so a
    # temporary repository removed on the way out races a process the caller has
    # no handle on. Injected through the environment rather than added to each
    # argv: these are the only two knobs, and the argv sites are many.
    # `maintenance.auto` is git >= 2.48; `gc.auto` is the older knob, and the
    # fallback newer git consults when the first is unset. Both, or the guard
    # holds on one version of git and not the other.
    "GIT_CONFIG_COUNT": "2",
    "GIT_CONFIG_KEY_0": "maintenance.auto",
    "GIT_CONFIG_VALUE_0": "false",
    "GIT_CONFIG_KEY_1": "gc.auto",
    "GIT_CONFIG_VALUE_1": "0",
}
GIT_ENV = dict(os.environ, **GIT_SCRUB)

# git's approxidate completes a bare date with the CURRENT time of day: the same
# pinned tree counted 274 commits before 10:00 and 278 at 22:00. Checked on the
# arguments at runtime, because a check reading this file's source certifies a
# spelling — `"--" + "sin" + "ce="` walks straight past one.
DATE_OPTS = ("since", "until", "after", "before", "max-age", "min-age")

# Reads that answer from the commit GRAPH rather than from the pinned tree. They
# depend on the clone's depth, its graft file and its replace refs — never on the
# SHA. A `--depth=1` clone made a repository of 5 502 commits report a squashed
# import, and the document asserted it.
WALK_SUBS = ("rev-list", "shortlog", "merge-base", "describe", "name-rev", "cherry")
WALK_OPTS = ("merged", "no-merged", "ancestry-path", "contains", "no-contains",
             "skip", "grep", "author", "committer", "first-parent", "walk-reflogs")


class Refusal(Exception):
    """Loud and named. The alternative is a framing that looks complete."""


def option_name(arg):
    return arg[2:].split("=", 1)[0] if arg.startswith("--") else ""


def flags_of(args):
    """The FLAGS of an invocation: what follows `--` is data, and so is any value
    that does not start with a dash. Matching every token made `tag -l 'rev-list'`
    look like a graph read."""
    out = []
    for a in args[1:]:
        if a == "--":
            break
        if a.startswith("-"):
            out.append(a)
    return out


def is_walk(args):
    sub = args[0] if args else ""
    flags = flags_of(args)
    if sub in WALK_SUBS or any(option_name(f) in WALK_OPTS for f in flags):
        return True
    if sub != "log":
        return False
    if "-1" not in flags:
        return True
    # `-1` bounds the OUTPUT, not the walk: a range still walks end to end.
    return any(".." in a for a in args[1:] if not a.startswith("-"))


def git(repo, *args, binary=False, walked=False):
    dated = [a for a in args if option_name(a) in DATE_OPTS]
    if dated:
        raise Refusal("date option handed to git (%s): approxidate completes a bare date with the "
                      "current time of day, so the same pinned tree answers differently by the hour"
                      % ", ".join(dated))
    if is_walk(args) and not walked:
        raise Refusal("graph read outside git_walk: git %s — the clone's depth would decide the "
                      "result, so the full history must be proven first" % " ".join(args[:2]))
    unscrubbed = sorted(k for k, v in GIT_SCRUB.items() if GIT_ENV.get(k) != v)
    if unscrubbed:
        raise Refusal("git environment not neutralised (%s): the user's configuration would decide "
                      "the output" % ", ".join(unscrubbed))
    p = subprocess.run(["git", "-C", repo, *args], capture_output=True, timeout=600, env=GIT_ENV)
    if p.returncode != 0:
        raise Refusal("git %s failed (%d): %s" % (" ".join(args[:3]), p.returncode,
                                                  p.stderr.decode("utf-8", "replace").strip()[-300:]))
    return p.stdout if binary else p.stdout.decode("utf-8", "replace")


_WALKABLE = set()


def git_walk(repo, *args):
    """A graph read, once the clone is proven to carry the whole graph."""
    if repo not in _WALKABLE:
        if git(repo, "rev-parse", "--is-shallow-repository").strip() == "true":
            raise Refusal("%s is a shallow clone: a graph read there measures the clone's depth, not "
                          "the history — the framing would report a squashed import of a full "
                          "repository" % repo)
        graft = os.path.join(repo, git(repo, "rev-parse", "--git-path", "info/grafts").strip())
        if os.path.exists(graft):
            raise Refusal("%s carries a graft file (%s): reachability there is no longer the SHA's"
                          % (repo, graft))
        _WALKABLE.add(repo)
    return git(repo, *args, walked=True)


def resolve(repo, ref, expect=None):
    sha = git(repo, "rev-parse", "--verify", "%s^{commit}" % ref).strip()
    if re.fullmatch(r"[0-9a-f]{40}", ref) and sha != ref:
        raise Refusal("%s: %s does not resolve to itself" % (repo, ref))
    if expect and sha != expect:
        raise Refusal("%s: %s now points at %s, pinned at %s — the upstream reference moved under "
                      "the framing" % (repo, ref, sha[:12], expect[:12]))
    return sha


def ls_tree(repo, sha):
    out = git(repo, "ls-tree", "-r", "-z", "--name-only", sha, binary=True)
    return [p.decode("utf-8", "replace") for p in out.split(b"\0") if p]


def cat_batch(repo, sha, paths):
    """One `git cat-file --batch` for N blobs. Missing or non-blob → None."""
    if not paths:
        return {}
    p = subprocess.Popen(["git", "-C", repo, "cat-file", "--batch"], env=GIT_ENV,
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    out, err = p.communicate("".join("%s:%s\n" % (sha, path) for path in paths).encode("utf-8"))
    if p.returncode != 0:
        raise Refusal("cat-file --batch: %s" % err.decode("utf-8", "replace")[-300:])
    res, pos = {}, 0
    for path in paths:
        nl = out.index(b"\n", pos)
        header = out[pos:nl].decode("utf-8", "replace").split()
        pos = nl + 1
        if header[-1] == "missing":
            res[path] = None
            continue
        kind, size = header[1], int(header[2])
        # A tree is not content: reading its raw object as a file made a
        # directory answer "the file exists".
        res[path] = out[pos:pos + size] if kind == "blob" else None
        pos += size + 1
    return res


def glob_re(pattern):
    out, i = "", 0
    while i < len(pattern):
        if pattern.startswith("**/", i):
            out += "(?:.*/)?"
            i += 3
        elif pattern.startswith("**", i):
            out += ".*"
            i += 2
        elif pattern[i] == "*":
            out += "[^/]*"
            i += 1
        elif pattern[i] == "?":
            out += "[^/]"
            i += 1
        else:
            out += re.escape(pattern[i])
            i += 1
    return re.compile("^" + out + "$")


def matching(files, pattern):
    rx = glob_re(pattern)
    return [f for f in files if rx.match(f)]


def canonical(item):
    return json.dumps(item, sort_keys=True, ensure_ascii=False) if isinstance(item, (dict, list)) else str(item)


def norm_path(p):
    """One spelling per path. `./vendor/x`, `vendor//x` and `vendor/x/` name the
    same files, and three spellings of one exclusion are three declarations the
    duplicate guard never sees — dedup on raw text is dedup on typography."""
    if not isinstance(p, str):
        raise Refusal("a path must be a string, got %s" % type(p).__name__)
    out = p.strip().replace("\\", "/")
    while "//" in out:
        out = out.replace("//", "/")
    while out.startswith("./"):
        out = out[2:]
    out = out.rstrip("/")
    if not out:
        raise Refusal("empty path in the declared scope — a claim about nothing proves nothing")
    if out.startswith("/") or ".." in out.split("/"):
        raise Refusal("path leaves the repository: %r — the perimeter is the tree at the pinned SHA, "
                      "and nothing outside it is evidence" % p)
    return out


def canonical_scope(scope):
    """The perimeter in normal form: one spelling per path, one order, no key
    nobody reads. The writer emits this and the linter refuses anything else, so
    a declaration cannot hide from the duplicate guard behind a second spelling.

    It is also the whole shape check: an exclusion is {glob, reason}, a claim is
    {name, evidence_file}, and a missing half is refused here rather than read as
    empty downstream."""
    if not isinstance(scope, dict):
        raise Refusal("the scope must be an object, got %s" % type(scope).__name__)
    unknown = sorted(set(scope) - {"excludes", "claims", "tag_pattern"})
    if unknown:
        raise Refusal("unknown key in the scope: %s — a key nobody reads is a declaration nobody "
                      "checks, and it would travel to the documents unexamined" % ", ".join(unknown))
    out = {}

    excludes = []
    for e in scope.get("excludes") or []:
        if not isinstance(e, dict):
            raise Refusal("an exclusion is an object {glob, reason}, got %s" % type(e).__name__)
        reason = str(e.get("reason", "")).strip()
        if not reason:
            raise Refusal("exclusion %r declared without a reason — an exclusion removes files from "
                          "every published figure, so it says on whose authority" % e.get("glob", ""))
        excludes.append({"glob": norm_path(e.get("glob", "")), "reason": reason})
    if excludes:
        out["excludes"] = sorted(excludes, key=lambda x: (x["glob"], x["reason"]))

    claims = []
    for c in scope.get("claims") or []:
        if not isinstance(c, dict):
            raise Refusal("a claim is an object {name, evidence_file}, got %s" % type(c).__name__)
        name = str(c.get("name", "")).strip()
        if not name:
            raise Refusal("a claim without a name — it would be counted and never read")
        claims.append({"name": name, "evidence_file": norm_path(c.get("evidence_file", ""))})
    if claims:
        out["claims"] = sorted(claims, key=lambda x: (x["name"], x["evidence_file"]))

    pattern = str(scope.get("tag_pattern", "")).strip()
    if pattern:
        out["tag_pattern"] = pattern
    # Here, and not only at the checking sites: normalising is what MAKES two
    # spellings of one declaration comparable, and a writer that emits a
    # perimeter it knows is invalid has already written it to disk by the time
    # anything downstream objects. Refused, never silently collapsed — folding
    # four declarations into one is a silent repair of the very thing guarded.
    refuse_duplicate_declarations(out)
    return out


def serialise(scope):
    """The one byte-sequence a perimeter has. The audit copy on disk is compared
    against this, so "what the reader recomputes from" and "what was measured"
    are the same object rather than two renderings of one intention."""
    return json.dumps(scope, ensure_ascii=False, sort_keys=True, indent=2) + "\n"


def refuse_duplicate_declarations(scope):
    """A declaration is a FIGURE, and the same claim made twice is counted twice —
    the evidence never contradicts it, because every copy points at the same real
    file. Measured on this bot's predecessor: one deployable declared four times
    moved the amplitude index from 3,84 to 4,20 and the published letter from M
    to L, with the freshness check still green."""
    dupes = []
    for key, items in scope.items():
        if not isinstance(items, list):
            continue
        seen = set()
        for item in items:
            c = canonical(item)
            if c in seen:
                dupes.append("%s: %s" % (key, c[:70]))
            seen.add(c)
    if dupes:
        raise Refusal("repeated declaration in the scope (%d) — a declaration is a figure: repeating "
                      "one inflates a count the tree never contradicts: %s"
                      % (len(dupes), "; ".join(sorted(set(dupes))[:3])))


def refuse_unproven_claims(repo, sha, scope, files):
    """Every claim carries the file that proves it, and that file must be in the
    tree at the pinned SHA. A claim whose evidence is absent is refused, never
    skipped: a skipped claim is a silent zero."""
    missing = []
    for claim in scope.get("claims", []):
        path = claim.get("evidence_file", "")
        if path not in files:
            missing.append("%s → %s" % (claim.get("name", "?"), path or "(no evidence declared)"))
    if missing:
        raise Refusal("declared claim without evidence in the tree (%d): %s"
                      % (len(missing), "; ".join(missing[:3])))


def refuse_empty_globs(scope, files):
    """A glob that matches nothing excludes nothing, and silently. Either the
    perimeter is wrong or the tree is — both deserve to be said."""
    empty = [e["glob"] for e in scope.get("excludes", []) if not matching(files, e["glob"])]
    if empty:
        raise Refusal("declared exclusion matching no file (%d): %s — an exclusion that excludes "
                      "nothing is a claim about a tree that does not hold"
                      % (len(empty), ", ".join(sorted(empty)[:3])))


def refuse_redundant_exclusions(scope, files):
    """An exclusion removing no file another exclusion does not already remove is
    a repeated declaration one indirection away: two globs, one set of files, a
    perimeter described twice. The duplicate guard cannot see it — the two
    spellings differ — so it is checked against the tree instead of against the
    text."""
    excludes = scope.get("excludes", [])
    sets = [set(matching(files, e["glob"])) for e in excludes]
    union_all = set().union(*sets) if sets else set()
    redundant = []
    for i, own in enumerate(sets):
        others = union_all - own if len(sets) == 1 else set().union(
            *(t for j, t in enumerate(sets) if j != i))
        if own and not (own - others):
            redundant.append(excludes[i]["glob"])
    if redundant:
        raise Refusal("exclusion removing no file another exclusion does not already remove (%d): %s "
                      "— the perimeter is described twice, and a description is a figure"
                      % (len(redundant), ", ".join(sorted(redundant)[:3])))


def refuse_empty_perimeter(files, perimeter):
    """Excluding everything makes every figure zero, every gate green and every
    document confident. The silent zero is the failure this bot exists against,
    and an over-broad glob is the cheapest way to produce one."""
    if files and not perimeter:
        raise Refusal("the declared exclusions removed all %d tracked files — a framing of an empty "
                      "perimeter reports zero everywhere and contradicts nothing" % len(files))


def scope_write(survey, repo, out_dir):
    """The surveyor's declarations made into the perimeter every later figure
    depends on.

    An agent node cannot write this file — the delegate runs read-only — and it
    must not: normalising and refusing are the two things a prompt cannot be
    held to. The agent says WHERE to look; what gets counted is decided here.

    A stack declared twice is refused for the same reason a claim is: it doubles
    a coverage denominator that no tree contradicts."""
    if not isinstance(survey, dict):
        raise Refusal("the survey output must be an object, got %s" % type(survey).__name__)

    raw = {"excludes": [], "claims": []}
    for d in survey.get("declarations") or []:
        if not isinstance(d, dict):
            raise Refusal("a declaration is an object, got %s" % type(d).__name__)
        kind = str(d.get("kind", "")).strip()
        if kind == "exclusion":
            raw["excludes"].append({"glob": d.get("glob", ""), "reason": d.get("reason", "")})
        elif kind == "claim":
            raw["claims"].append({"name": d.get("name", ""), "evidence_file": d.get("evidence_file", "")})
        else:
            raise Refusal("declaration of unknown kind %r — the surveyor declares exclusions and "
                          "claims, and anything else is a fact no gate would ever check" % kind)
    pattern = str(survey.get("tag_pattern", "") or "").strip()
    if pattern:
        raw["tag_pattern"] = pattern
    scope = canonical_scope(raw)

    stacks, seen = [], set()
    if not isinstance(survey.get("stacks") or [], list):
        raise Refusal("stacks must be a list, got %s" % type(survey.get("stacks")).__name__)
    for st in survey.get("stacks") or []:
        if not isinstance(st, dict):
            raise Refusal("a detected stack is an object {id, …}, got %s" % type(st).__name__)
        sid = str(st.get("id", "")).strip()
        if not sid:
            raise Refusal("a detected stack without an id — an unnamed stack matches no skill and "
                          "names no extractor, so coverage could never be checked over it")
        if sid in seen:
            raise Refusal("stack %r detected twice — a repeated stack is a repeated declaration: it "
                          "inflates the coverage denominator without adding a single artifact" % sid)
        seen.add(sid)
        stacks.append(dict(st, id=sid))
    stacks.sort(key=lambda x: x["id"])

    unresolved = survey.get("unresolved") or []
    if not isinstance(unresolved, list):
        raise Refusal("unresolved must be a list, got %s" % type(unresolved).__name__)

    rel = norm_path(out_dir) + "/scope.json"
    dst = os.path.join(repo, rel)
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    with open(dst, "w", encoding="utf-8") as f:
        f.write(serialise(scope))
    return {"scope": scope, "stacks": stacks, "unresolved": unresolved, "audit_path": rel}


def scope_lint(repo, ref, scope, audit_path=None, expect=None):
    """The perimeter checked against the tree it claims to describe.

    Every refusal here is repeated by the floor, on purpose: this node exists to
    name WHICH declaration is wrong while the run is still cheap to fix, not to
    be the thing that makes the figures safe.

    The audit copy is compared byte for byte against the perimeter that will
    actually be measured. A reader recomputes a figure from the file on disk; if
    that file is not the object the floor consumed, they get a different answer
    and no one finds out."""
    if canonical_scope(scope) != scope:
        raise Refusal("the perimeter is not in normal form — it carries a spelling the duplicate "
                      "guard cannot see through. Regenerate it; do not edit it by hand")
    sha = resolve(repo, ref, expect)
    files = ls_tree(repo, sha)
    refuse_duplicate_declarations(scope)
    refuse_unproven_claims(repo, sha, scope, files)
    refuse_empty_globs(scope, files)
    refuse_redundant_exclusions(scope, files)

    partition, excluded = [], set()
    for e in scope.get("excludes", []):
        hit = matching(files, e["glob"])
        excluded.update(hit)
        partition.append({"glob": e["glob"], "reason": e["reason"], "files": len(hit),
                          "sample": sorted(hit)[:3]})
    perimeter = [f for f in files if f not in excluded]
    refuse_empty_perimeter(files, perimeter)

    if audit_path:
        try:
            on_disk = open(os.path.join(repo, audit_path), encoding="utf-8").read()
        except OSError as e:
            raise Refusal("the audit copy of the perimeter is unreadable at %s: %s" % (audit_path, e))
        if on_disk != serialise(scope):
            raise Refusal("the audit copy at %s is not the perimeter that will be measured — a reader "
                          "recomputing a figure from it would get a different answer, and nothing "
                          "would say so" % audit_path)

    return {"sha": sha, "tracked": len(files), "excluded": len(excluded),
            "perimeter": len(perimeter), "partition": partition,
            "claims": [c["name"] for c in scope.get("claims", [])]}


def floor(repo, ref, scope, expect=None):
    """The always-on layer. It must produce output on ANY repository: an absence
    here means this extractor failed, which is not the same as an empty tree."""
    sha = resolve(repo, ref, expect)
    files = ls_tree(repo, sha)
    # Re-run the perimeter guards rather than trust that scope_lint ran: this
    # body is also the one an operator calls on a hand-written scope, and a
    # guarantee that depends on an upstream node is a guarantee about a graph.
    scope = canonical_scope(scope)
    refuse_duplicate_declarations(scope)
    refuse_unproven_claims(repo, sha, scope, files)
    refuse_empty_globs(scope, files)
    refuse_redundant_exclusions(scope, files)

    excluded = set()
    for e in scope.get("excludes", []):
        excluded.update(matching(files, e["glob"]))
    perimeter = [f for f in files if f not in excluded]
    refuse_empty_perimeter(files, perimeter)

    # Extensions are DISCOVERED, never enumerated: this file has no opinion about
    # which of them matter, and a repository in a language nobody here has heard
    # of is described exactly like any other.
    by_ext = {}
    for f in perimeter:
        base = f.rsplit("/", 1)[-1]
        ext = base.rsplit(".", 1)[-1].lower() if "." in base[1:] else ""
        by_ext[ext] = by_ext.get(ext, 0) + 1

    blobs = cat_batch(repo, sha, perimeter)
    lines, binary_files = 0, 0
    for f in perimeter:
        b = blobs.get(f)
        if b is None:
            continue
        if b"\0" in b[:8000]:
            binary_files += 1
            continue
        lines += b.count(b"\n") + (0 if b.endswith(b"\n") or not b else 1)

    total = int(git_walk(repo, "rev-list", "--count", sha).strip())
    head_ts = int(git(repo, "log", "-1", "--format=%ct", sha).strip())
    stamps = [int(x) for x in git_walk(repo, "log", "--format=%ct", sha).split() if x]
    floor_ts = head_ts - 180 * 86400
    recent = sum(1 for ts in stamps if floor_ts < ts <= head_ts) if total > 1 else None

    pattern = scope.get("tag_pattern", "*")
    reachable = [t for t in git_walk(repo, "tag", "--merged", sha, "--sort=creatordate", "-l", pattern).split() if t]
    all_tags = [t for t in git(repo, "tag", "-l", pattern).split() if t]

    return {
        "sha": sha,
        "date": git(repo, "log", "-1", "--format=%cs", sha).strip(),
        "files_tracked": len(files),
        "files_excluded": len(excluded),
        "files_perimeter": len(perimeter),
        "files_binary": binary_files,
        "lines_perimeter": lines,
        "by_extension": dict(sorted(by_ext.items())),
        "commits_total": total,
        # None, not 0: "we cannot measure it" and "there is none" are different
        # answers, and the second one is a lie when the history was squashed.
        "commits_recent_180d": recent,
        "tags_reachable": len(reachable),
        "tags_at_pattern": len(all_tags),
        "claims_proven": len(scope.get("claims", [])),
    }


def envelope(fn):
    """Refusals leave by the same door as results, so the workflow can route
    them: a JSON envelope and exit 0, never a non-zero exit the runtime would
    turn into an opaque tool error. The callee names its own fields — three
    nodes share this door and none of them means the same thing by success."""
    try:
        out = fn()
        res = {"ok": True, "refused": False, "reason": ""}
        res.update(out if isinstance(out, dict) else {"result": out})
        return res
    except Refusal as e:
        return {"ok": False, "refused": True, "reason": str(e)}


def selftest():
    """Every named refusal, fired at an input it must refuse. A guard nobody has
    seen refuse is a guard nobody can trust — and the count below says exactly
    what it proves: N guards seen refusing N examples, not N guards proven.

    That every `raise Refusal` in this file is reached by one of the probes below
    is itself checkable, and checked: `--falsify` neutralises each one in turn and
    demands this bench redden. A probe list maintained by hand drifts behind the
    code it covers; that harness answers from the code."""
    import tempfile
    fired, survivors = [], []

    def probe(name, fn, fragment):
        try:
            fn()
        except Refusal as e:
            if fragment not in str(e):
                survivors.append("%s — another refusal answered: %s" % (name, str(e)[:90]))
            else:
                fired.append(name)
            return
        except Exception as e:
            survivors.append("%s — %s instead of a named refusal" % (name, type(e).__name__))
            return
        survivors.append("%s — no refusal" % name)

    with tempfile.TemporaryDirectory() as tmp:
        repo = os.path.join(tmp, "r")
        os.makedirs(repo)
        env = dict(GIT_ENV, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t",
                   GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t")
        with open(os.path.join(repo, "a.txt"), "w", encoding="utf-8") as f:
            f.write("x\n")
        for cmd in (["init", "-q"], ["add", "-A"], ["commit", "-qm", "one"]):
            subprocess.run(["git", "-C", repo] + cmd, check=True, capture_output=True, env=env)
        sha = resolve(repo, "HEAD")

        shallow = os.path.join(tmp, "s")
        subprocess.run(["git", "clone", "-q", "--depth=1", "file://" + repo, shallow],
                       check=True, capture_output=True, env=env)

        probe("git: command failed", lambda: resolve(repo, "no-such-ref"), "failed")
        probe("git: date option", lambda: git(repo, "log", "-1", "--since=2020-01-01"), "date option")
        probe("git: date option built in pieces",
              lambda: git(repo, "log", "-1", "--" + "un" + "til", "2020-01-01"), "date option")
        probe("graph read outside git_walk", lambda: git(repo, "rev-list", "--count", sha), "outside git_walk")
        probe("graph read: --skip bounds output not walk",
              lambda: git(repo, "log", "-1", "--skip=5", "--format=%H"), "outside git_walk")
        probe("shallow clone", lambda: git_walk(shallow, "rev-list", "--count", "HEAD"), "shallow clone")
        probe("reference moved", lambda: resolve(repo, "HEAD", "0" * 40), "moved under")
        probe("declaration repeated", lambda: refuse_duplicate_declarations(
            {"claims": [{"name": "a", "evidence_file": "a.txt"}, {"name": "a", "evidence_file": "a.txt"}]}),
            "repeated declaration")
        probe("claim without evidence", lambda: refuse_unproven_claims(
            repo, sha, {"claims": [{"name": "a", "evidence_file": "absent.txt"}]}, ["a.txt"]),
            "without evidence")
        probe("exclusion matching nothing", lambda: refuse_empty_globs(
            {"excludes": [{"glob": "**/*.nothing", "reason": "r"}]}, ["a.txt"]), "matching no file")
        probe("path leaving the repository", lambda: norm_path("../etc/hosts"), "leaves the repository")
        probe("exclusion without a reason",
              lambda: canonical_scope({"excludes": [{"glob": "a/**"}]}), "without a reason")
        probe("unknown key in the perimeter", lambda: canonical_scope({"exclude": []}), "unknown key")
        probe("perimeter described twice", lambda: refuse_redundant_exclusions(
            {"excludes": [{"glob": "*.txt", "reason": "r"}, {"glob": "a.*", "reason": "r"}]},
            ["a.txt"]), "removing no file")
        probe("perimeter emptied by a glob", lambda: refuse_empty_perimeter(["a.txt"], []), "removed all")
        probe("declaration of unknown kind", lambda: scope_write(
            {"declarations": [{"kind": "note", "text": "x"}]}, repo, "d"), "unknown kind")
        probe("stack detected twice", lambda: scope_write(
            {"stacks": [{"id": "s"}, {"id": "s"}]}, repo, "d"), "detected twice")
        probe("perimeter not in normal form", lambda: scope_lint(
            repo, "HEAD", {"excludes": [{"glob": "./a.txt", "reason": "r"}]}), "normal form")
        with open(os.path.join(repo, "audit.json"), "w", encoding="utf-8") as f:
            f.write("{}\n")
        probe("audit copy is not what will be measured", lambda: scope_lint(
            repo, "HEAD", {"claims": [{"name": "a", "evidence_file": "a.txt"}]}, "audit.json"),
            "not the perimeter that will be measured")

        probe("path that is not a string", lambda: norm_path(1), "must be a string")
        probe("path that is only blanks", lambda: norm_path("  "), "empty path")
        probe("perimeter that is not an object", lambda: canonical_scope([]), "must be an object")
        probe("exclusion given as bare text",
              lambda: canonical_scope({"excludes": ["a/**"]}), "an exclusion is an object")
        probe("claim given as bare text",
              lambda: canonical_scope({"claims": ["a"]}), "a claim is an object")
        probe("claim without a name",
              lambda: canonical_scope({"claims": [{"evidence_file": "a.txt"}]}), "without a name")
        probe("survey that is not an object", lambda: scope_write([], repo, "d"), "must be an object")
        probe("declaration given as bare text",
              lambda: scope_write({"declarations": ["x"]}, repo, "d"), "a declaration is an object")
        probe("stacks given as text",
              lambda: scope_write({"stacks": "one"}, repo, "d"), "stacks must be a list")
        probe("stack given as bare text",
              lambda: scope_write({"stacks": ["x"]}, repo, "d"), "a detected stack is an object")
        probe("stack without an id",
              lambda: scope_write({"stacks": [{"name": "x"}]}, repo, "d"), "without an id")
        probe("unresolved given as text",
              lambda: scope_write({"unresolved": "x"}, repo, "d"), "unresolved must be a list")
        probe("audit copy absent", lambda: scope_lint(
            repo, "HEAD", {"claims": [{"name": "a", "evidence_file": "a.txt"}]}, "absent.json"),
            "unreadable")

        # The git-level refusals, exercised through the real plumbing rather than
        # through a double: each one below runs `git` for real, in the state the
        # guard exists to refuse.
        grafted = os.path.join(tmp, "g")
        subprocess.run(["git", "clone", "-q", "file://" + repo, grafted],
                       check=True, capture_output=True, env=env)
        graft_file = os.path.join(grafted, ".git", "info", "grafts")
        os.makedirs(os.path.dirname(graft_file), exist_ok=True)
        open(graft_file, "w", encoding="utf-8").write(sha + "\n")
        probe("graft file", lambda: git_walk(grafted, "rev-list", "--count", "HEAD"), "graft file")

        def unscrubbed_env():
            saved = GIT_ENV["GIT_CONFIG_GLOBAL"]
            GIT_ENV["GIT_CONFIG_GLOBAL"] = os.path.join(tmp, "hostile.gitconfig")
            try:
                return git(repo, "log", "-1", "--format=%H")
            finally:
                GIT_ENV["GIT_CONFIG_GLOBAL"] = saved
        probe("git environment not neutralised", unscrubbed_env, "not neutralised")

        subprocess.run(["git", "-C", repo, "tag", "-a", "-m", "t", "v0"],
                       check=True, capture_output=True, env=env)
        tag_object = subprocess.run(["git", "-C", repo, "rev-parse", "v0"], check=True,
                                    capture_output=True, env=env).stdout.decode().strip()
        # A tag OBJECT sha is 40 hex and is not the commit: pinning one looks
        # pinned and moves whenever the tag is re-cut.
        probe("tag object pinned as a commit", lambda: resolve(repo, tag_object),
              "does not resolve to itself")
        probe("cat-file over something that is not a repository",
              lambda: cat_batch(os.path.join(tmp, "nowhere"), sha, ["a.txt"]), "cat-file --batch")

        probe("one thing declared four times", lambda: scope_write(
            {"declarations": [{"kind": "claim", "name": "d", "evidence_file": "a.txt"}] * 4},
            repo, "d"), "repeated declaration")
        probe("one exclusion in three spellings", lambda: scope_write(
            {"declarations": [{"kind": "exclusion", "glob": g, "reason": "r"}
                              for g in ("a/**", "./a/**", "a//**")]}, repo, "d"),
            "repeated declaration")

        written = scope_write({"declarations": [{"kind": "claim", "name": "runtime",
                                                 "evidence_file": "./a.txt"}],
                               "stacks": [{"id": "b"}, {"id": "a"}], "unresolved": ["target version"]},
                              repo, "docs/x")

        checks = [
            # the guards must not refuse what they exist to allow
            refuse_duplicate_declarations({"claims": [{"name": "a", "f": "D"}, {"name": "b", "f": "D"}]}) is None,
            refuse_unproven_claims(repo, sha, {"claims": [{"name": "a", "evidence_file": "a.txt"}]}, ["a.txt"]) is None,
            refuse_empty_globs({"excludes": [{"glob": "**/*.txt", "reason": "r"}]}, ["a.txt"]) is None,
            # a partial overlap is legitimate: two concerns may share a file, and
            # only subsumption means one of them describes nothing new
            refuse_redundant_exclusions({"excludes": [{"glob": "*.txt", "reason": "r"},
                                                      {"glob": "b.*", "reason": "r"}]},
                                        ["a.txt", "b.txt", "b.md"]) is None,
            # normal form is a fixed point, and paths have exactly one spelling
            norm_path("./a//b/") == "a/b",
            canonical_scope(canonical_scope(written["scope"])) == written["scope"],
            # what the writer emits, the linter accepts and the floor measures —
            # the same object at all three, not three readings of one intention
            written["scope"] == {"claims": [{"name": "runtime", "evidence_file": "a.txt"}]},
            written["audit_path"] == "docs/x/scope.json",
            [st["id"] for st in written["stacks"]] == ["a", "b"],
            scope_lint(repo, "HEAD", written["scope"], written["audit_path"])["perimeter"] == 1,
            floor(repo, "HEAD", written["scope"])["files_perimeter"] == 1,
            # the audit copy on disk IS the perimeter, byte for byte
            open(os.path.join(repo, written["audit_path"]), encoding="utf-8").read()
            == serialise(written["scope"]),
            # the envelope carries the callee's own fields, whatever they are
            envelope(lambda: {"scope": {}})["ok"] is True,
            # auto-maintenance is refused, asked of git itself rather than of the
            # dict: what matters is the config git RESOLVES, not what was set
            git(repo, "config", "--get", "maintenance.auto").strip() == "false",
            git(repo, "config", "--get", "gc.auto").strip() == "0",
            # a walk is decided on the FLAGS, never on a value
            is_walk(("rev-list", "--count", "S")) is True,
            is_walk(("tag", "--merged", "S")) is True,
            is_walk(("log", "-1", "--skip=5")) is True,
            is_walk(("tag", "-l", "rev-list")) is False,
            is_walk(("ls-tree", "-r", "S", "--", "merge-base")) is False,
            is_walk(("log", "-1", "--format=%cs", "S")) is False,
            option_name("--since=X") == "since" and option_name("-1") == "",
            # the floor answers on a repository it knows nothing about
            floor(repo, "HEAD", {})["files_perimeter"] == 1,
            floor(repo, "HEAD", {})["lines_perimeter"] == 1,
            floor(repo, "HEAD", {})["by_extension"] == {"txt": 1},
            # one commit: "not measurable" is not "none"
            floor(repo, "HEAD", {})["commits_recent_180d"] is None,
            # a refusal leaves as an envelope, not as a crash
            envelope(lambda: floor(shallow, "HEAD", {}))["refused"] is True,
            envelope(lambda: {"floor": floor(repo, "HEAD", {})})["ok"] is True,
        ]

    failed = [i for i, ok in enumerate(checks) if not ok]
    if survivors or failed:
        print("selftest FAILED", file=sys.stderr)
        for s in survivors:
            print("  unfalsified: " + s, file=sys.stderr)
        if failed:
            print("  checks failed: %s" % failed, file=sys.stderr)
        return 1
    print("selftest: %d checks green, %d guards seen refusing %d examples — a guard refuses ITS "
          "example, its domain is not proven" % (len(checks), len(fired), len(fired)))
    return 0


# ---- shared body above ----

# The bench launches 83 git invocations today. This is a FLOOR against a blinded
# detector, not a target: shrink the selftest freely, but a collapse to a handful
# means the hook stopped seeing, and that must not read as success.
MIN_GIT_INVOCATIONS = 40

# Read ONCE, at load. A check that re-reads its own source at the moment of
# concluding certifies whatever the name points at by then: a selftest that
# reloaded its file after running reported 43/43 guards falsified against a
# binary that no longer existed.
_SRC = open(__file__, encoding="utf-8").read()


def falsify():
    """Neutralise each `raise Refusal` in turn and demand the bench redden.

    This is what makes the selftest's count mean something. A probe list kept by
    hand drifts behind the code: measured here, ten guards were exercised and
    seventeen more refusal sites were not, and nothing in a green run said so.
    The sites are enumerated from the parsed source rather than from a list, so
    a refusal added tomorrow is covered or it is reported."""
    import ast
    import tempfile
    sites = sorted((n.lineno, n.end_lineno, n.col_offset) for n in ast.walk(ast.parse(_SRC))
                   if isinstance(n, ast.Raise) and isinstance(n.exc, ast.Call)
                   and getattr(n.exc.func, "id", "") == "Refusal")
    lines = _SRC.split("\n")
    survivors = []
    with tempfile.TemporaryDirectory() as tmp:
        mutant_path = os.path.join(tmp, "mutant.py")
        for lo, hi, col in sites:
            with open(mutant_path, "w", encoding="utf-8") as f:
                f.write("\n".join(lines[:lo - 1] + [" " * col + "pass"] + lines[hi:]))
            r = subprocess.run([sys.executable, mutant_path, "--selftest"], capture_output=True)
            if r.returncode == 0:
                survivors.append("line %d: %s" % (lo, lines[lo - 1].strip()[:80]))
    for line in survivors:
        print("  UNEXERCISED " + line, file=sys.stderr)
    print("%d refusals neutralised one at a time, %d survived a green bench"
          % (len(sites), len(survivors)), file=sys.stderr if survivors else sys.stdout)
    return 1 if survivors else 0


def audited_selftest():
    """Run the bench with every git subprocess it launches recorded, and demand
    each one resolve the auto-maintenance knobs.

    Enforced by EXECUTION, not by reading: the site that escaped two careful
    readings of a sibling bundle was a helper defined inside a block, four
    hundred lines from the guarded one. A reading enumerates; it does not
    converge.

    Two deliberate choices, each closing a spelling:

    - the hook is `Popen`, not `run` — `run`, `check_output`, `call` and
      `check_call` all funnel through it, so a future call written with any of
      them is seen. (`os.system` would not be; nothing here uses it, and this
      refuses if that changes.)
    - EVERY git invocation is checked, not only those under a temporary
      directory. Scoping by directory would mean matching its name, and a
      helper that spells its temp dir differently walks straight past — the
      same failure, one level up."""
    seen, offenders = [], []

    def resolved(argv, env):
        # `env=None` is not an empty environment: it means the child INHERITS the
        # parent's, and the inherited one is what git will actually read. Judging
        # it against {} calls a compliant inherited call an offender — the audit
        # must answer about the environment that runs, like everything else here.
        environ = os.environ if env is None else env
        flags = [argv[i + 1] for i, a in enumerate(argv[:-1]) if a == "-c"]
        keys = {environ.get("GIT_CONFIG_KEY_%d" % i): environ.get("GIT_CONFIG_VALUE_%d" % i)
                for i in range(int(environ.get("GIT_CONFIG_COUNT", 0) or 0))}
        return (("maintenance.auto=false" in flags or keys.get("maintenance.auto") == "false")
                and ("gc.auto=0" in flags or keys.get("gc.auto") == "0"))

    real_popen, real_system = subprocess.Popen, os.system
    allowed = {"git", os.path.basename(sys.executable)}

    def spy(args, *a, **kw):
        argv = list(args) if isinstance(args, (list, tuple)) else [args]
        prog = os.path.basename(str(argv[0])) if argv else ""
        # An ALLOWLIST, not a list of shells to refuse. `sh -c "git …"` and
        # `shell=True` both hide the argv from this hook — measured: either one
        # runs an unguarded git command while the audit reports its usual count,
        # unchanged and green. Blocking the shells I can name would leave the one
        # I cannot; permitting only the programs this bench actually launches
        # leaves nothing to name.
        if kw.get("shell"):
            offenders.append("shell=True %r — the hook sees a string, not an argv, and what runs "
                             "inside it is unobservable here" % str(args)[:60])
        elif prog not in allowed:
            offenders.append("%s — under the audit only %s may be launched; a program that can "
                             "start git is a path around this hook"
                             % (prog or "(empty argv)", ", ".join(sorted(allowed))))
        elif prog == "git":
            seen.append(argv)
            if not resolved(argv, kw.get("env")):
                offenders.append(" ".join(map(str, argv[:6])))
        return real_popen(args, *a, **kw)

    def refuse_system(cmd):
        offenders.append("os.system(%r) — outside every subprocess hook" % cmd[:60])
        return real_system(cmd)

    subprocess.Popen, os.system = spy, refuse_system
    try:
        rc = selftest()
    finally:
        subprocess.Popen, os.system = real_popen, real_system
    if rc:
        return rc
    if offenders:
        print("git subprocess audit FAILED — %d of %d invocations do not refuse auto-maintenance:"
              % (len(offenders), len(seen)), file=sys.stderr)
        for o in sorted(set(offenders))[:6]:
            print("  " + o, file=sys.stderr)
        return 1
    # Two conditions, not one. "No offender" is also what a blinded hook says: an
    # edit that stops Popen from being wrapped, or a selftest that stops running
    # git, both read as a clean tree. The floor is a floor and not a coverage
    # measure — it exists so that the detector losing its grip is louder than the
    # thing it detects.
    if len(seen) < MIN_GIT_INVOCATIONS:
        print("git subprocess audit has lost its grip: %d invocations observed, floor is %d. "
              "An empty offender list from a hook that never fired is indistinguishable from a "
              "clean one." % (len(seen), MIN_GIT_INVOCATIONS), file=sys.stderr)
        return 1
    print("git subprocess audit: %d invocations, all refusing auto-maintenance — observed at "
          "Popen, not read from the source" % len(seen))
    return 0


def main(argv):
    if "--selftest" in argv:
        return audited_selftest()
    if "--falsify" in argv:
        return selftest() or falsify()
    args = {}
    for i, a in enumerate(argv):
        if a.startswith("--") and i + 1 < len(argv):
            args[a[2:]] = argv[i + 1]
    known = ("repo", "ref", "sha", "scope", "survey", "out-dir", "audit", "mode", "selftest")
    unknown = [a for a in argv if a.startswith("--") and a[2:] not in known]
    if unknown:
        # An unrecognised flag used to be ignored: a misspelt gate stayed green
        # having verified nothing.
        print(json.dumps({"ok": False, "refused": True,
                          "reason": "unrecognised argument: %s" % ", ".join(unknown)}))
        return 0
    repo, ref, mode = args.get("repo", "."), args.get("ref", "HEAD"), args.get("mode", "floor")
    scope = json.loads(open(args["scope"], encoding="utf-8").read()) if args.get("scope") else {}
    if mode == "write":
        survey = json.loads(open(args["survey"], encoding="utf-8").read()) if args.get("survey") else {}
        out = envelope(lambda: scope_write(survey, repo, args.get("out-dir", "docs/modernisation")))
    elif mode == "lint":
        out = envelope(lambda: scope_lint(repo, ref, scope, args.get("audit"), args.get("sha")))
    elif mode == "floor":
        out = envelope(lambda: {"floor": floor(repo, ref, scope, args.get("sha"))})
    else:
        out = {"ok": False, "refused": True,
               "reason": "unknown mode %r — one of: write, lint, floor" % mode}
    print(json.dumps(out, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
