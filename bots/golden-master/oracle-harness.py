#!/usr/bin/env python3
"""Deterministic oracle harness — the bot-owned half of the golden-master bot.

Copy kept standalone for review. The executable copy is inlined in main.bot's
`oracle_run` tool node; keep the two in sync when either changes.

WHAT THIS PROGRAM DECIDES, AND WHY IT IS NOT THE AGENT'S TO WRITE
-----------------------------------------------------------------
The campaign agent writes WHAT is compared: the corpus, the canonicalisation
rules, the comparators, the mutants. This program writes HOW the verdict is
reached. If the agent owned both, a comparator that returns "identical" for
everything would score a perfect run — the exact failure the consultancy shipped for a whole
milestone (a PDF comparator that validated pages with not one character on
them, so a word changed in the database still went green).

An oracle is accepted here only if it is falsifiable in BOTH directions:

  it MUST see a known injected divergence   → kills the blind judge
  it MUST stay silent on a null mutation    → kills the hysterical judge

A comparator that always reports "different" trivially detects every mutant;
the no-op control is what stops it passing. A comparator that always reports
"identical" detects nothing; the mutation score is what stops it passing.
Neither check alone is sufficient. Together they are a proof of non-vacuity.

Four locks keep the mutant set honest, because the agent writes the mutants too:

  1. Required archetypes come from the skill as DATA, not from agent judgement.
     harness_gate refuses a set that omits one for a surface class in use.
  2. Mechanical validity: a mutant whose apply.sh changes nothing is INVALID,
     not "undetected" — it can neither inflate nor dilute the score.
  3. The adversarial lens proposes evasive mutants without seeing the existing
     ones.
  4. The held-out set is sealed: never shown to the hardening loop, never in a
     fail log, scored exactly once at the final gate. Without it the loop
     overfits — the agent hardens the comparator until it catches precisely the
     mutants it can see, and the oracle goes green on its own training set.

Stdlib only, deliberately: no venv, no pip, no network install. The oracle must
run in any sandbox, including one with no egress.
"""

import hashlib
import http.cookiejar
import importlib.util
import json
import os
import shlex
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# ─── Layout of .golden-master/ in the target repo ───────────────────────────
#
#   config.json      how to bring the app up/down, base_url, personas   (agent)
#   corpus.json      the request catalogue                              (agent)
#   canon/rules.py   canonicalize(entry, status, headers, body) -> str  (agent)
#   refs/<id>.txt    captured references                                (agent)
#   mutants/<id>/    apply.sh, revert.sh, meta.json                     (agent)
#   mutants/holdout/<id>/                                               (agent)
#   REPORT.md, verify-oracle.sh                                    (emitted)

# Required archetypes per surface class. Deliberately embedded HERE rather than
# read from the skill: the skill guides the agent, this constant binds it. An
# enforcement rule the author of the mutants can edit is not an enforcement
# rule. `skills/oracle-mutation.md` documents each entry and why it exists —
# keep the two in sync, but this one decides.
REQUIRED_ARCHETYPES = {
    "http":   ["value_change", "order_flip", "subset", "status_change", "field_drop"],
    "binary": ["content_empty", "value_change"],
    "screen": ["style_shift"],
    "asset":  ["content_change"],
}

CONTROL_SAMPLE = 6          # non-target entries replayed per mutant, for collateral
BOOT_TIMEOUT_S = 300
CMD_TIMEOUT_S = 1800


def log(msg):
    print(msg, file=sys.stderr, flush=True)


def run(cmd, cwd, timeout=CMD_TIMEOUT_S):
    """Run a shell command, returning (exit_code, combined_output)."""
    try:
        p = subprocess.run(
            ["sh", "-c", cmd], cwd=cwd, stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT, text=True, timeout=timeout,
        )
        return p.returncode, p.stdout or ""
    except subprocess.TimeoutExpired as e:
        return 124, "timeout after %ss: %s" % (timeout, (e.output or "")[-500:])
    except OSError as e:
        return 127, "could not execute: %s" % e


# ─── Agent-authored canonicalisation, loaded as a module ────────────────────

def load_canon(gm_dir):
    path = os.path.join(gm_dir, "canon", "rules.py")
    if not os.path.isfile(path):
        raise SystemExit("canon/rules.py is missing — the campaign must write it")
    spec = importlib.util.spec_from_file_location("gm_canon", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    if not hasattr(mod, "canonicalize"):
        raise SystemExit("canon/rules.py defines no canonicalize(entry, status, headers, body)")
    return mod


# ─── HTTP capture ───────────────────────────────────────────────────────────

class Session:
    """One persona's HTTP session: cookie jar + form login."""

    def __init__(self, base_url):
        self.base_url = base_url.rstrip("/")
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar)
        )

        class _NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *_a, **_kw):
                return None

        self.no_redirect_opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar), _NoRedirect()
        )

    def fetch(self, method, path, fields=None, timeout=60, follow=True):
        """follow=False captures the redirect ITSELF.

        An authorisation refusal that answers 302 -> /login records the login
        page as its reference when redirects are followed: stable, plausible,
        and blind to the refusal it was meant to cover.
        """
        url = self.base_url + path
        data = None
        headers = {"Accept": "*/*", "User-Agent": "iterion-golden-master/1"}
        if fields:
            data = urllib.parse.urlencode(fields).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        opener = self.opener if follow else self.no_redirect_opener
        try:
            with opener.open(req, timeout=timeout) as r:
                return r.status, dict(r.headers), r.read()
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers or {}), e.read()
        except Exception as e:
            return 0, {}, ("TRANSPORT-ERROR: %s" % e).encode()

    def login(self, spec):
        """Form login. Re-reads any CSRF token the form exposes first.

        Defensive on a missing token: a baseline may have no CSRF protection at
        all while its modernised target does. Requiring a token here would make
        the harness unusable on the very baseline it must capture.
        """
        path = spec.get("path", "/login")
        fields = dict(spec.get("fields", {}))
        token_field = spec.get("csrf_field")
        if token_field:
            _, _, body = self.fetch("GET", path)
            tok = extract_input_value(body, token_field)
            if tok:
                fields[token_field] = tok
        status, _, _ = self.fetch(spec.get("method", "POST"), path, fields=fields)
        return status


def extract_input_value(body, name):
    """Pull `value` out of <input name="<name>" value="…">. No HTML parser: the
    harness must not depend on how well-formed the page is."""
    try:
        text = body.decode("utf-8", "replace")
    except Exception:
        return None
    needle = 'name="%s"' % name
    i = text.find(needle)
    if i < 0:
        return None
    seg = text[max(0, i - 300): i + 300]
    k = seg.find('value="')
    if k < 0:
        return None
    return seg[k + 7: seg.find('"', k + 7)]




def resolve_base_url(config, ws):
    """Resolve the application's base URL, preferring a PUBLISHED one.

    A recorded `base_url` is only valid on the machine and path that recorded
    it. Environments that derive their ports (to avoid two checkouts fighting
    over one) publish the effective URL to a file, and a net that bakes the
    literal instead cannot run anywhere else — the same orphaned-artefact
    failure as a harness that does not travel, one level subtler because the
    net looks complete and simply never reaches the application.

    `base_url_file` wins when present and readable; `base_url` remains the
    right answer for a genuinely fixed endpoint.
    """
    rel = config.get("base_url_file")
    if rel:
        path = rel if os.path.isabs(rel) else os.path.join(ws, rel)
        try:
            with open(path, encoding="utf-8") as f:
                url = f.read().strip()
            if url:
                return url
        except OSError as e:
            raise SystemExit(
                "config.base_url_file points at %s, which cannot be read (%s). "
                "That file is written when the environment comes up; either the "
                "app was never started, or the path is wrong for this checkout."
                % (path, e))
    url = config.get("base_url")
    if not url:
        raise SystemExit("config.json declares neither base_url nor base_url_file")
    return url


def open_sessions(config):
    sessions = {}
    for p in config.get("personas", [{"name": "anon"}]):
        s = Session(resolve_base_url(config, os.environ.get("GM_WORKSPACE", ".")))
        if p.get("login"):
            code = s.login(p["login"])
            if code >= 400:
                raise SystemExit(
                    "persona %r could not log in (HTTP %s) — a capture taken with a "
                    "broken session records the login page as every reference"
                    % (p["name"], code)
                )
        sessions[p["name"]] = s
    return sessions


def capture(config, corpus, canon, ids=None):
    """Fetch and canonicalise the corpus. Returns {id: canonical_text}."""
    sessions = open_sessions(config)
    out = {}
    for e in corpus["entries"]:
        if ids is not None and e["id"] not in ids:
            continue
        s = sessions.get(e.get("persona", "anon"))
        if s is None:
            raise SystemExit("entry %s names unknown persona %r" % (e["id"], e.get("persona")))
        status, headers, body = s.fetch(
            e.get("method", "GET"), e["path"], fields=e.get("fields"),
            follow=not e.get("no_redirect", False),
        )
        out[e["id"]] = canon.canonicalize(e, status, headers, body)
    return out


# ─── Application lifecycle ──────────────────────────────────────────────────

def app_up(config, ws):
    code, out = run(config["up"], ws, timeout=BOOT_TIMEOUT_S)
    if code != 0:
        raise SystemExit("config.up failed (exit %s):\n%s" % (code, out[-2000:]))
    base = resolve_base_url(config, ws).rstrip("/")
    ready = config.get("ready_path", "/")
    deadline = time.time() + BOOT_TIMEOUT_S
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(base + ready, timeout=5) as r:
                if r.status < 500:
                    return
        except Exception:
            time.sleep(1)
    raise SystemExit("application never answered %s%s within %ss" % (base, ready, BOOT_TIMEOUT_S))


def app_down(config, ws):
    if config.get("down"):
        run(config["down"], ws, timeout=BOOT_TIMEOUT_S)


def app_restart(config, ws):
    app_down(config, ws)
    app_up(config, ws)


# ─── Mutants ────────────────────────────────────────────────────────────────

def missing_archetypes(corpus, mutants):
    """Surface classes in the corpus that no mutant covers for a required archetype.

    Checked BEFORE the application is booted: a mutant set with a hole in it
    cannot produce a meaningful figure, so there is no point paying for a run.
    """
    have = {}
    for m in mutants:
        have.setdefault(m.get("surface", "http"), set()).add(m.get("archetype"))
    gaps = []
    for surface in sorted({e.get("surface", "http") for e in corpus["entries"]}):
        for archetype in REQUIRED_ARCHETYPES.get(surface, []):
            if archetype not in have.get(surface, set()):
                gaps.append({"surface": surface, "archetype": archetype})
    return gaps




def sealed_dir_for(ws):
    """THE rule for where a held-out set lives once sealed. One implementation.

    An earlier fix made the campaign and the gate derive this path from the same
    rule instead of one dictating it to the other. The very next commit added a
    third party -- the node that promotes a spent set to published evidence --
    with its own hand-written copy of the derivation. It pointed elsewhere,
    promoted nothing, and reported success saying "nothing was held out, or it
    was already promoted": a message that makes the failure look like a
    legitimate state.

    Duplicating a rule is what broke it the first time. Callers import this.
    """
    explicit = os.environ.get("GM_SEALED_DIR")
    if explicit:
        return explicit
    root = os.environ.get("GM_SCRATCH", os.path.join(ws, ".."))
    return os.path.join(root, "gm-holdout-" + (os.path.basename(os.path.abspath(ws)) or "default"))


def seal_holdout(gm_dir, sealed_dir):
    """Move the held-out set OUT of the worktree, once, at the first gate.

    The seal was a sentence in a skill and nothing enforced it: on the first
    real run the campaign simply executed the held-out mutants itself, learned
    which ones escaped, and could then harden against them — which is exactly
    the overfitting the set exists to prevent. Relocating them makes the seal
    mechanical from the second pass onward, which is where hardening actually
    compounds.

    Returns True when the set now lives outside the workspace.
    """
    src = os.path.join(gm_dir, "mutants", "holdout")
    os.makedirs(sealed_dir, exist_ok=True)
    if os.path.isdir(src):
        for name in sorted(os.listdir(src)):
            target = os.path.join(sealed_dir, name)
            if not os.path.exists(target):
                shutil.move(os.path.join(src, name), target)
        try:
            os.rmdir(src)
        except OSError:
            pass
    return bool(os.listdir(sealed_dir))




def mutant_fingerprint(d):
    """Identify a mutant by WHAT IT DOES, not by its name.

    A held-out mutant that has already been scored is spent: its secrecy is a
    property of the moment, not of the artefact, and once the gate has judged
    it its value flips entirely to evidence. Spent sets are therefore promoted
    to mutants/audit/ and committed, so a third party can replay the claim
    "N/N detected" instead of taking it on trust — the exact thing that makes a
    delivery's own reported figures worthless.

    That only holds if each hardening cycle draws a FRESH set. Renaming a spent
    mutant would defeat it, so identity is the change itself: the applied
    script plus the entries it targets.
    """
    parts = []
    for fname in ("apply.sh", "revert.sh"):
        p = os.path.join(d, fname)
        if os.path.isfile(p):
            with open(p, "rb") as f:
                parts.append(f.read())
    meta_path = os.path.join(d, "meta.json")
    targets = []
    if os.path.isfile(meta_path):
        try:
            with open(meta_path, encoding="utf-8") as f:
                targets = sorted(json.load(f).get("targets") or [])
        except (ValueError, OSError):
            pass
    parts.append(("|".join(targets)).encode("utf-8"))
    return hashlib.sha256(b"\x00".join(parts)).hexdigest()


def spent_fingerprints(gm_dir):
    """Fingerprints of every held-out set already scored and committed."""
    root = os.path.join(gm_dir, "mutants", "audit")
    out = {}
    if not os.path.isdir(root):
        return out
    for cycle in sorted(os.listdir(root)):
        cdir = os.path.join(root, cycle)
        if not os.path.isdir(cdir):
            continue
        for name in sorted(os.listdir(cdir)):
            d = os.path.join(cdir, name)
            if os.path.isdir(d):
                out[mutant_fingerprint(d)] = cycle + "/" + name
    return out


def load_mutants(gm_dir, holdout, sealed_dir=None):
    if holdout:
        root = sealed_dir or os.path.join(gm_dir, "mutants", "holdout")
        if not os.path.isdir(root):
            return []
        out = []
        for name in sorted(os.listdir(root)):
            d = os.path.join(root, name)
            meta_path = os.path.join(d, "meta.json")
            if not os.path.isdir(d) or not os.path.isfile(meta_path):
                continue
            with open(meta_path, encoding="utf-8") as f:
                meta = json.load(f)
            meta["id"], meta["dir"] = name, d
            out.append(meta)
        return out
    root = os.path.join(gm_dir, "mutants", "")
    if not os.path.isdir(root):
        return []
    out = []
    for name in sorted(os.listdir(root)):
        d = os.path.join(root, name)
        if name == "holdout" or not os.path.isdir(d):
            continue
        meta_path = os.path.join(d, "meta.json")
        if not os.path.isfile(meta_path):
            continue
        with open(meta_path, encoding="utf-8") as f:
            meta = json.load(f)
        meta["id"] = name
        meta["dir"] = d
        out.append(meta)
    return out


def tree_fingerprint(ws):
    code, out = run("git status --porcelain", ws, timeout=120)
    return out if code == 0 else None


def data_fingerprint(meta, ws):
    """Optional probe for mutants that change data rather than files."""
    probe = meta.get("fingerprint_cmd")
    if not probe:
        return None
    _, out = run(probe, ws, timeout=300)
    return hashlib.sha256(out.encode()).hexdigest()


def run_script(path, ws, timeout=600):
    """Run a mutant script HONOURING ITS SHEBANG.

    Forcing `sh` here silently substitutes the interpreter: on most systems
    /bin/sh is dash, which has no `source`, so a script written for bash fails
    with a bare `exit 127` and a "command not found" for whatever the sourced
    file was meant to define. The author sees a broken mutant and no hint that
    the shell was swapped under them. Observed on the first real run.
    """
    if os.access(path, os.X_OK):
        return run(shlex.quote(path), ws, timeout)
    return run("sh %s" % shlex.quote(path), ws, timeout)


def apply_mutant(meta, ws):
    return run_script(os.path.join(meta["dir"], "apply.sh"), ws)


def revert_mutant(meta, ws):
    return run_script(os.path.join(meta["dir"], "revert.sh"), ws)


# ─── Comparison ─────────────────────────────────────────────────────────────

def diverged(refs, captured, ids):
    return sorted(i for i in ids if refs.get(i) != captured.get(i))


def control_ids(corpus, targets, seed):
    """Deterministic sample of non-target entries, to measure collateral."""
    pool = [e["id"] for e in corpus["entries"] if e["id"] not in targets]
    if not pool:
        return []
    step = max(1, len(pool) // CONTROL_SAMPLE)
    return pool[seed % max(1, step)::step][:CONTROL_SAMPLE]


def score_mutant(meta, config, corpus, canon, refs, ws, seed):
    """Apply → capture → compare → revert. Returns a per-mutant verdict dict."""
    targets = list(meta.get("targets") or [])
    if not targets:
        return {"id": meta["id"], "valid": False, "detected": False,
                "reason": "meta.json declares no targets"}

    before_tree, before_data = tree_fingerprint(ws), data_fingerprint(meta, ws)

    code, out = apply_mutant(meta, ws)
    if code != 0:
        return {"id": meta["id"], "valid": False, "detected": False,
                "reason": "apply.sh exited %s: %s" % (code, out[-300:])}

    after_tree, after_data = tree_fingerprint(ws), data_fingerprint(meta, ws)
    mutated = (before_tree != after_tree) or (
        before_data is not None and before_data != after_data
    )

    verdict = {"id": meta["id"], "class": meta.get("class", "unknown"),
               "archetype": meta.get("archetype", "unknown"),
               "surface": meta.get("surface", "http"),
               "targets_declared": targets, "valid": bool(mutated)}

    if not mutated:
        # A mutant that changes nothing is a measurement fault, not evidence.
        verdict.update(detected=False,
                       reason="apply.sh left the tree and the data probe unchanged")
        revert_mutant(meta, ws)
        return verdict

    if meta.get("needs_restart", True):
        app_restart(config, ws)

    sample = control_ids(corpus, set(targets), seed)
    captured = capture(config, corpus, canon, ids=set(targets) | set(sample))
    moved = diverged(refs, captured, targets)
    verdict["detected"] = bool(moved)
    # Per-target, not per-mutant. `targets` is a contract: every declared entry
    # MUST move. A mutant that moves one target and leaves three untouched is
    # still "detected" in aggregate while three references are provably blind
    # to that class of change — which is exactly how the consultancy shipped four public
    # search fixtures that could not see a changed offer.
    verdict["undetected_targets"] = [t for t in targets if t not in moved]
    # Collateral is usually not a harness fault but an under-declared blast
    # radius: the mutant really does move that response. Naming the entries is
    # what makes the finding actionable instead of a bare count.
    verdict["collateral"] = diverged(refs, captured, sample)
    # How many control entries the collateral check actually compared. A mutant
    # that declares the whole corpus as targets leaves nothing to control
    # against, and its "collateral: 0" is vacuous rather than earned — so the
    # coverage is reported and gated, not assumed.
    verdict["control_covered"] = len(sample)

    code, out = revert_mutant(meta, ws)
    if code != 0:
        verdict["revert_clean"] = False
        verdict["reason"] = "revert.sh exited %s: %s" % (code, out[-300:])
        return verdict

    if meta.get("needs_restart", True):
        app_restart(config, ws)
    back = capture(config, corpus, canon, ids=set(targets))
    verdict["revert_clean"] = not diverged(refs, back, targets)
    if not verdict["revert_clean"]:
        verdict["reason"] = "the tree did not return to the reference after revert.sh"
    return verdict


def score_noop(config, corpus, canon, refs, ws, seed):
    """The negative control, built BY THE HARNESS so the agent cannot shape it.

    Nothing is mutated. Every comparator must stay silent. A comparator that
    reports a difference here is non-deterministic or unconditionally noisy,
    and its greens on real mutants mean nothing.

    It covers the WHOLE corpus, and that is a correction rather than a
    refinement. It used to sample the first six entries, which left every later
    entry never once confronted with its own reference — a reference could be
    stale, or frozen against a world that has since moved, and nothing would
    say so unless a mutant happened to target it.

    That is not hypothetical. A modernisation run on a different DAY replayed a
    net whose entry 013 rendered a seeded creation date: the seed used the
    database's current_date(), so the reference had frozen on the day the
    fixture was first applied. The drift was permanent and invisible to both
    guards — stability compares captures to EACH OTHER, all taken the same day,
    and the negative control's prefix did not reach entry 013. It surfaced only
    as collateral noise under unrelated mutants, blamed on a lot that had not
    caused it.

    Whole-corpus costs one extra pass of plain requests against an application
    that is already up, which is nothing next to a reference nobody checks.
    """
    ids = [e["id"] for e in corpus["entries"]]
    app_restart(config, ws)
    captured = capture(config, corpus, canon)
    noisy = diverged(refs, captured, ids)
    return {"silent": not noisy, "noisy_entries": noisy}


# ─── Stability ──────────────────────────────────────────────────────────────

def stability(config, corpus, canon, ws):
    """A/B on one boot, C after a full restart.

    A vs B catches per-request non-determinism. B vs C catches per-boot
    non-determinism and anything that leaks wall-clock: real time elapses
    between them, which is a truer drift probe than a frozen fake clock.
    """
    a = capture(config, corpus, canon)
    b = capture(config, corpus, canon)
    ab = sorted(i for i in a if a[i] != b.get(i))
    app_restart(config, ws)
    c = capture(config, corpus, canon)
    bc = sorted(i for i in b if b[i] != c.get(i))
    return {"stable": not ab and not bc, "ab_drift": ab, "bc_drift": bc}, c


# ─── Entry point ────────────────────────────────────────────────────────────

def main():
    ws = os.environ.get("GM_WORKSPACE", ".")
    gm_dir = os.path.join(ws, os.environ.get("GM_DIR", ".golden-master"))
    # The seal must be run-scoped, and campaign and gate must land on the SAME
    # directory. They are different processes and only one of them can be given
    # an environment, so the path is DERIVED rather than passed: the workspace
    # basename is the run id inside an iterion worktree, and a stable repo name
    # outside one. A selfcheck that seals therefore cannot strand the held-out
    # set somewhere the gate will not look, and two runs cannot share a pile.
    sealed_dir = sealed_dir_for(ws)
    floor = int(os.environ.get("GM_MUTATION_FLOOR", "90"))
    mode = os.environ.get("GM_MODE", "gate")   # gate | record | selfcheck

    report = {"mode": mode, "total": 0, "valid": 0, "detected": 0, "score_pct": 0,
              "noop_silent": False, "revert_clean": True, "collateral": 0,
              "notice": "", "uncontrolled": [], "blind_lanes": [], "missing_archetypes": [],
              "holdout_detected": 0, "holdout_total": 0, "stable": False,
              "corpus_total": 0, "corpus_distinct": 0, "duplicate_refs": [],
              "runner_replayable": False,
              "holdout_reused": [],
              "log_tail": ""}

    def bail(msg):
        report["log_tail"] = msg
        print(json.dumps(report))
        raise SystemExit(0)

    for required in ("config.json", "corpus.json"):
        if not os.path.isfile(os.path.join(gm_dir, required)):
            bail("%s is missing — the campaign has not produced an oracle yet" % required)

    with open(os.path.join(gm_dir, "config.json"), encoding="utf-8") as f:
        config = json.load(f)
    with open(os.path.join(gm_dir, "corpus.json"), encoding="utf-8") as f:
        corpus = json.load(f)

    try:
        canon = load_canon(gm_dir)
    except SystemExit as e:
        bail(str(e))

    refs_dir = os.path.join(gm_dir, "refs")
    os.makedirs(refs_dir, exist_ok=True)

    # The harness materialises itself into the target repo. Three things need
    # it there and none of them can reach the bundle: the emitted
    # verify-oracle.sh, a CI job, and the campaign on a later pass wanting to
    # (re)record with the code path that will judge it. Copying from __file__
    # keeps one source of truth — the inlined node — rather than a second copy
    # drifting in a sibling node.
    try:
        shutil.copyfile(__file__, os.path.join(gm_dir, "harness.py"))
    except (OSError, NameError):
        pass

    # Une porte sur un arbre sale juge un arbre qui n'a jamais existé : les
    # captures partent de ce qui est là, puis le premier revert de mutant —
    # `git checkout -- <fichier>` — ramène ces fichiers à HEAD, et tout ce qui
    # est capturé ensuite décrit autre chose. Le travail non committé est
    # détruit au passage, en silence. Signalé plutôt que refusé : enregistrer et
    # itérer sur un arbre sale est légitime, GATER dessus ne l'est pas.
    if mode != "record":
        dirty = subprocess.run(["git", "-C", ws, "status", "--porcelain"],
                               capture_output=True, text=True)
        if dirty.returncode == 0 and dirty.stdout.strip():
            paths = [l[3:] for l in dirty.stdout.strip().splitlines()]
            report["notice"] = ((report["notice"] + " | ") if report["notice"] else "") + (
                "WORKSPACE NOT COMMITTED (%d path(s): %s). Mutant reverts restore HEAD, so "
                "these changes are destroyed during the run and the verdict below describes "
                "a tree that never existed. Commit, then gate."
                % (len(paths), ", ".join(sorted(paths)[:12])))

    visible = load_mutants(gm_dir, holdout=False)
    if mode != "record":
        seal_holdout(gm_dir, sealed_dir)
        held_meta = load_mutants(gm_dir, holdout=True, sealed_dir=sealed_dir)
        # The held-out set lives outside the workspace once sealed. If the
        # sealed directory moved or was wiped, it is GONE — and the archetype
        # check below would then blame the campaign for an archetype it did
        # write, sending it off to duplicate work that already exists. Say what
        # actually happened instead.
        # Three ways there is no held-out set to score, and they are NOT the
        # same event. Conflating them is how a vacuous 0 == 0 slips through the
        # conjunction wearing a green coat.
        holdout_spent = bool(spent_fingerprints(gm_dir))
        if not held_meta and not os.path.isdir(os.path.join(gm_dir, "mutants", "holdout")):
            if holdout_spent:
                # Legitimate: this cycle's set was scored once and published as
                # evidence. The blindness proof was MADE and is replayable from
                # mutants/audit/ — it is simply not being re-made here, and the
                # report must say so rather than let 0 == 0 read as a pass.
                report["notice"] = ("the held-out set for this cycle is SPENT and published "
                                    "under mutants/audit/. This replay re-checks the visible "
                                    "counter-test only; the held-out figure below is 0/0 and "
                                    "proves nothing on its own. Draw a fresh set to harden "
                                    "again — the gate refuses one that repeats a published "
                                    "fingerprint.")
            else:
                bail("the sealed held-out set is missing from %s and no longer in the "
                     "workspace. It was relocated by an earlier gate and the sealed "
                     "directory has since changed or been cleared; GM_SEALED_DIR must be "
                     "stable across passes. Restore it, or re-create the held-out set "
                     "knowing the previous seal is broken." % sealed_dir)
        gaps = missing_archetypes(corpus, visible + held_meta)
        if gaps:
            report["missing_archetypes"] = gaps
            bail("no mutant covers these required (surface, archetype) pairs: %s. Each "
                 "archetype is blind to a DIFFERENT comparator defect; an uncovered one "
                 "leaves that defect undetectable, so the run would report a figure it "
                 "has not earned. See skills/oracle-mutation.md."
                 % json.dumps(gaps, ensure_ascii=False))

    try:
        app_up(config, ws)
    except SystemExit as e:
        bail(str(e))

    try:
        if mode == "record":
            snap = capture(config, corpus, canon)
            for k, v in snap.items():
                with open(os.path.join(refs_dir, k + ".txt"), "w", encoding="utf-8") as f:
                    f.write(v)
            # Record mode returns the gate skeleton with its default values, so
            # `noop_silent: false, score_pct: 0` reads exactly like a failed gate
            # to anyone glancing at it. The mode field and an unambiguous tail
            # are what tell the two apart.
            report["log_tail"] = "MODE=record — %d references written. No gate was run: the " \
                                 "zeroed fields above are defaults, not a verdict." % len(snap)
            print(json.dumps(report))
            return

        refs = {}
        for e in corpus["entries"]:
            p = os.path.join(refs_dir, e["id"] + ".txt")
            if not os.path.isfile(p):
                bail("no reference for entry %s — record before gating" % e["id"])
            with open(p, encoding="utf-8") as f:
                refs[e["id"]] = f.read()

        # Width is a property of what the net can SEE, so it is counted on
        # DISTINCT references, never on entries. Two entries whose canonical
        # reference is byte-identical are ONE observation: a mutant that moves
        # one moves the other, so the second buys no coverage while inflating
        # the count. Seen on a real third-party net — two distinct export
        # endpoints, one reference, and the single behaviour that told them
        # apart was captured nowhere. Duplicates are reported, not banned:
        # genuinely redundant endpoints are legal, they just stop counting
        # twice. The FLOOR itself is applied by the gate, not here — the
        # harness states facts, the graph decides.
        by_hash = {}
        for eid, text in refs.items():
            h = hashlib.sha256(text.encode("utf-8")).hexdigest()
            by_hash.setdefault(h, []).append(eid)
        report["corpus_total"] = len(refs)
        report["corpus_distinct"] = len(by_hash)
        report["duplicate_refs"] = sorted(
            sorted(ids) for ids in by_hash.values() if len(ids) > 1)

        stab, _ = stability(config, corpus, canon, ws)
        report.update(stable=stab["stable"])
        if not stab["stable"]:
            bail("capture is not deterministic; A/B drift on %s, B/C drift on %s — "
                 "canonicalise these before any mutation figure means anything"
                 % (stab["ab_drift"][:8], stab["bc_drift"][:8]))

        noop = score_noop(config, corpus, canon, refs, ws, 0)
        report["noop_silent"] = noop["silent"]

        verdicts, blind = [], []
        for seed, meta in enumerate(visible):
            v = score_mutant(meta, config, corpus, canon, refs, ws, seed)
            verdicts.append(v)
            if not v.get("valid"):
                continue
            if not v.get("detected"):
                blind.append({"surface": v.get("surface"), "archetype": v.get("archetype"),
                              "mutant_id": v["id"], "entries": v.get("targets_declared", []),
                              "why": "no declared target moved"})
            elif v.get("undetected_targets"):
                blind.append({"surface": v.get("surface"), "archetype": v.get("archetype"),
                              "mutant_id": v["id"], "entries": v["undetected_targets"],
                              "why": "these references did not move for a change they cover"})

        # The seal relocates the held-out set in BOTH modes — the campaign must
        # lose file access early — but selfcheck neither scores it nor reports
        # it. Revealing `holdout_detected` to whoever runs the check is enough to
        # steer hardening: seeing 3/5 says "keep tuning" even with the files out
        # of reach. The held-out result belongs to the final gate alone.
        held = [] if mode == "selfcheck" else [
            score_mutant(m, config, corpus, canon, refs, ws, 1000 + i)
            for i, m in enumerate(held_meta)]

        valid = [v for v in verdicts if v.get("valid")]
        detected = [v for v in valid if v.get("detected")]
        report.update(
            total=len(verdicts),
            valid=len(valid),
            detected=len(detected),
            score_pct=int(100 * len(detected) / len(valid)) if valid else 0,
            revert_clean=all(v.get("revert_clean", True) for v in verdicts + held),
            collateral=sum(len(v.get("collateral") or []) for v in verdicts),
            uncontrolled=[v["id"] for v in valid if not v.get("control_covered")],
            blind_lanes=blind,
            holdout_total=len([v for v in held if v.get("valid")]),
            holdout_detected=len([v for v in held if v.get("valid") and v.get("detected")]),
        )
        if mode == "selfcheck":
            # Withheld, not zero-because-failed. The two numbers are made
            # DELIBERATELY UNEQUAL: the gate converges on
            # `holdout_detected == holdout_total`, so a selfcheck report that
            # ever reached the gate must fail it rather than sail through on a
            # -1 == -1 coincidence.
            report["holdout_total"] = len(held_meta)
            report["holdout_detected"] = -1

        problems = []
        if not report["noop_silent"]:
            problems.append("NEGATIVE CONTROL FAILED: the no-op mutation made %s diverge. "
                            "A comparator that fires without a change proves nothing when it "
                            "fires with one." % noop["noisy_entries"][:8])
        for v in verdicts:
            if not v.get("valid"):
                problems.append("mutant %s is INVALID: %s" % (v["id"], v.get("reason", "")))
        if blind:
            problems.append("BLIND LANES — references that do NOT move for a change they "
                            "cover. Not averaged away, not weighted: this list must be "
                            "empty. %s" % json.dumps(blind, ensure_ascii=False))
        if report["collateral"]:
            detail = "; ".join(
                "%s moved %s" % (v["id"], v["collateral"])
                for v in verdicts if v.get("collateral")
            )
            problems.append("collateral drift on %d control entries — a mutant moves "
                            "responses it does not declare as targets. Either its "
                            "`targets` under-state its blast radius, or the capture is "
                            "not isolated: %s" % (report["collateral"], detail))
        if report["uncontrolled"]:
            problems.append("mutants %s declare the whole corpus as targets: nothing was "
                            "left to control against, so their clean collateral is vacuous. "
                            "A mutant must be NARROW — a blast radius covering everything "
                            "tests nothing precise." % report["uncontrolled"])
        if report["score_pct"] < floor:
            problems.append("mutation score %d%% is under the %d%% floor"
                            % (report["score_pct"], floor))
        if report["duplicate_refs"]:
            problems.append("%d reference group(s) are byte-identical across DIFFERENT entries, "
                            "so the corpus is %d observations wide, not %d. This is NOT always a "
                            "defect: on a refusal lane two entries legitimately capture the same "
                            "302, and the second is a control proving a mutant moved only the "
                            "first. It IS a defect when the endpoints were meant to differ — then "
                            "either one is redundant, or they differ on a path this fixture does "
                            "not exercise and the difference is captured NOWHERE. Decide which, "
                            "per group: %s"
                            % (len(report["duplicate_refs"]), report["corpus_distinct"],
                               report["corpus_total"],
                               json.dumps(report["duplicate_refs"], ensure_ascii=False)))
        if mode == "selfcheck":
            report["notice"] = ("MODE=selfcheck — the held-out set was sealed but NOT scored; "
                                "its result is withheld on purpose. Only the final gate scores "
                                "it. An empty log_tail here means the VISIBLE set is clean, "
                                "which is not the same as a green gate.")
        elif report["holdout_total"] and report["holdout_detected"] < report["holdout_total"]:
            problems.append("HELD-OUT set: %d/%d detected. The oracle was hardened against "
                            "the mutants it could see, not against divergence in general."
                            % (report["holdout_detected"], report["holdout_total"]))
        # Is the emitted net actually REPLAYABLE from a clean checkout? The
        # runner shells out to harness.py, which this gate materialises into
        # the worktree — but a campaign that gitignores it ships references
        # nobody can re-run. That is not a hypothetical: it is the exact
        # criticism levelled at third-party deliveries whose fixtures outlived
        # the harness that produced them, and the first emitted net here
        # reproduced it. A deliverable that cannot be replayed is a claim, not
        # an artefact.
        report["runner_replayable"] = True
        runner = os.path.join(gm_dir, "verify-oracle.sh")
        unanswerable = []
        for needed in (os.path.join(gm_dir, "harness.py"), runner):
            rel = os.path.relpath(needed, ws)
            if not os.path.isfile(needed):
                report["runner_replayable"] = False
                problems.append("%s is MISSING from the workspace: whatever produced "
                                "this tree did not carry the net's own machinery, so "
                                "nothing here can be replayed." % rel)
                continue
            ignored = subprocess.run(["git", "-C", ws, "check-ignore", "-q", rel],
                                     capture_output=True)
            if ignored.returncode == 0:
                report["runner_replayable"] = False
                problems.append("%s is GITIGNORED, so the committed net cannot be "
                                "replayed by CI or by anyone who checks the repo "
                                "out. The oracle is a deliverable: un-ignore it and "
                                "commit it." % rel)
            elif ignored.returncode != 1:
                # git answered neither "ignored" (0) nor "tracked-or-untracked"
                # (1): there is no repository here, or no git at all. The check
                # then discriminates NOTHING, and staying silent would let it
                # report the good outcome for the one reason it cannot see —
                # exactly the shape of failure this net exists to catch. Some
                # CI runners hand the job a COPY of the tracked files rather
                # than a checkout; presence is then real evidence, trackedness
                # is not, and the report says which of the two it has.
                unanswerable.append(rel)
        if unanswerable:
            report["notice"] = ((report["notice"] + " | ") if report["notice"] else "") + (
                "git could not be asked whether %s are ignored — this workspace is not a "
                "checkout (a copy, or no git available). They are PRESENT, which is what a "
                "copy of tracked files proves; that they are TRACKED is verified only where "
                "a checkout exists." % ", ".join(unanswerable))
        # A held-out set that repeats an already-spent one is not held out at
        # all: its mutants are committed under mutants/audit/ where anyone,
        # including the hardening loop, can read them. Reuse is refused by
        # FINGERPRINT rather than by name, so renaming does not launder it.
        spent = spent_fingerprints(gm_dir)
        report["holdout_reused"] = sorted(
            "%s (already scored as %s)" % (m["id"], spent[mutant_fingerprint(m["dir"])])
            for m in held_meta if mutant_fingerprint(m["dir"]) in spent)
        if report["holdout_reused"]:
            problems.append("the held-out set REPEATS mutants already scored and "
                            "published: %s. A spent set is evidence, not a test — "
                            "draw a fresh one, or the held-out figure measures "
                            "nothing the hardening loop could not already see."
                            % ", ".join(report["holdout_reused"]))
        report["log_tail"] = "\n".join(problems)[-6000:]
    finally:
        app_down(config, ws)

    print(json.dumps(report))


if __name__ == "__main__":
    main()
