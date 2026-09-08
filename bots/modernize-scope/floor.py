#!/usr/bin/env python3
"""The stack-agnostic floor of a framing pass.

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
    empty = [g for g in scope.get("excludes", []) if not matching(files, g)]
    if empty:
        raise Refusal("declared exclusion matching no file (%d): %s — an exclusion that excludes "
                      "nothing is a claim about a tree that does not hold"
                      % (len(empty), ", ".join(empty[:3])))


def floor(repo, ref, scope, expect=None):
    """The always-on layer. It must produce output on ANY repository: an absence
    here means this extractor failed, which is not the same as an empty tree."""
    sha = resolve(repo, ref, expect)
    files = ls_tree(repo, sha)
    refuse_duplicate_declarations(scope)
    refuse_unproven_claims(repo, sha, scope, files)
    refuse_empty_globs(scope, files)

    excluded = set()
    for g in scope.get("excludes", []):
        excluded.update(matching(files, g))
    perimeter = [f for f in files if f not in excluded]

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
    turn into an opaque tool error."""
    try:
        return {"ok": True, "refused": False, "floor": fn()}
    except Refusal as e:
        return {"ok": False, "refused": True, "reason": str(e)}


def selftest():
    """Every named refusal, fired at an input it must refuse. A guard nobody has
    seen refuse is a guard nobody can trust — and the count below says exactly
    what it proves: N guards seen refusing N examples, not N guards proven."""
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
            {"excludes": ["**/*.nothing"]}, ["a.txt"]), "matching no file")

        checks = [
            # the guards must not refuse what they exist to allow
            refuse_duplicate_declarations({"claims": [{"name": "a", "f": "D"}, {"name": "b", "f": "D"}]}) is None,
            refuse_unproven_claims(repo, sha, {"claims": [{"name": "a", "evidence_file": "a.txt"}]}, ["a.txt"]) is None,
            refuse_empty_globs({"excludes": ["**/*.txt"]}, ["a.txt"]) is None,
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
            envelope(lambda: floor(repo, "HEAD", {}))["ok"] is True,
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


def main(argv):
    if "--selftest" in argv:
        return selftest()
    args = {}
    for i, a in enumerate(argv):
        if a.startswith("--") and i + 1 < len(argv):
            args[a[2:]] = argv[i + 1]
    unknown = [a for a in argv if a.startswith("--") and a[2:] not in ("repo", "ref", "sha", "scope", "selftest")]
    if unknown:
        # An unrecognised flag used to be ignored: a misspelt gate stayed green
        # having verified nothing.
        print(json.dumps({"ok": False, "refused": True,
                          "reason": "unrecognised argument: %s" % ", ".join(unknown)}))
        return 0
    scope = json.loads(open(args["scope"], encoding="utf-8").read()) if args.get("scope") else {}
    out = envelope(lambda: floor(args.get("repo", "."), args.get("ref", "HEAD"), scope, args.get("sha")))
    print(json.dumps(out, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
