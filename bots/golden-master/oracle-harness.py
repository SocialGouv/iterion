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
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
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
    "asset":  ["content_change", "asset_missing"],
    "a11y":   ["violation_added"],
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


def resolve_artifact(config, ws):
    """Absolute path of the artefact IN FLIGHT, published by the environment.

    Same doctrine as `base_url_file`, for the same reason: a path baked into
    the net is valid on exactly one machine. The environment that builds and
    starts the application is the only thing that knows which file it started.
    """
    rel = config.get("artifact_file")
    if not rel:
        raise SystemExit(
            "an entry declares surface 'asset' but config.json has no "
            "artifact_file. That lane inventories what the BUILD packaged; "
            "without the artefact it would fall back to scanning the worktree, "
            "which is a different set — build outputs are gitignored, and stale "
            "ones from an earlier build stay behind.")
    path = rel if os.path.isabs(rel) else os.path.join(ws, rel)
    try:
        with open(path, encoding="utf-8") as f:
            jar = f.read().strip()
    except OSError as e:
        raise SystemExit(
            "config.artifact_file points at %s, which cannot be read (%s). It "
            "is written when the application starts." % (path, e))
    if not jar or not os.path.isfile(jar):
        raise SystemExit("the published artefact path %r is not a file" % jar)
    return jar


# URLs locales referencees par un gabarit. Deux formes cohabitent dans ce
# depot : l'attribut Thymeleaf `th:href="@{/chemin}"` et l'attribut HTML brut
# `href="/chemin"`. Les deux sont captees ; les URLs absolues et les ancres ne
# le sont pas, elles ne designent pas un fichier servi par cette application.
#
# Les espaces sont ADMISES dans la valeur. Les exclure paraissait prudent — un
# attribut mal ferme aurait absorbe la moitie de la balise — mais quatre
# documents de ce depot portent une espace dans leur nom de fichier, et le
# motif les ecartait sans le dire. Les guillemets bornent deja la valeur ; les
# accolades restent exclues, elles signalent une expression Thymeleaf calculee,
# dont l'URL n'est pas connue avant le rendu.
_ASSET_REF_RE = re.compile(
    r'(?:th:)?(?:href|src)\s*=\s*"(?:@\{)?(/[^"{}]+?)\}?"', re.I)

_ASSET_EXT = (".css", ".js", ".png", ".jpg", ".jpeg", ".gif", ".svg",
              ".ico", ".woff", ".woff2", ".ttf", ".eot", ".map", ".pdf")

# Le balisage commente n'est pas une reference : rien ne le sert au client. Sans
# ce retrait, quatre liens mis en commentaire remontaient comme des ressources
# attendues, dont une sous une forme de normalisation Unicode differente de
# celle empaquetee — un doublon fantome que la reference aurait fige.
_HTML_COMMENT_RE = re.compile(r"<!--.*?-->", re.S)


def _artefact_members(path):
    """(name, read()) for every file in the artefact — archive or directory.

    Both shapes are ordinary build outputs, and neither is more legitimate than
    the other. Accepting only one would push whoever has the other towards
    scanning the worktree instead, which is the single thing this lane must not
    do.
    """
    import zipfile
    if os.path.isdir(path):
        for root, _dirs, files in os.walk(path):
            for f in files:
                full = os.path.join(root, f)
                yield os.path.relpath(full, path).replace(os.sep, "/"), \
                    (lambda p=full: open(p, "rb").read())
        return
    with zipfile.ZipFile(path) as z:
        for name in z.namelist():
            if not name.endswith("/"):
                yield name, (lambda n=name: z.read(n))


def collect_assets(session, artefact_path, entry):
    """Inventory the assets the build PACKAGED, then ask the application for each.

    Two facts, deliberately kept apart:

      * what the artefact contains — read from the build output, the product;
      * what the application answers — read over HTTP, which is the truth.

    They are not the same claim, and a lane that reports only the first would
    describe a set of files without establishing that any of them is reachable.
    The comparison of the two is the whole point: a resource present in the
    package and answering 404 is a routing defect, and one answering 200 with
    other bytes is a filter or a cache rewriting the product on its way out.

    Referenced URLs come from the packaged TEMPLATES, for the same reason the
    inventory comes from the packaged static tree: a scan of the worktree would
    read sources the artefact may not contain.

    The two prefixes are DECLARED by the entry, never guessed. A default would
    have to encode one framework's layout, and the day it is wrong it does not
    fail — it inventories nothing and reports a clean, empty manifest.
    """
    static_prefix = entry.get("static_prefix")
    tpl_prefix = entry.get("template_prefix")
    if static_prefix is None or tpl_prefix is None:
        raise SystemExit(
            "entry %s declares surface 'asset' but not both `static_prefix` and "
            "`template_prefix` — the paths, inside the artefact, of the served "
            "tree and of the templates that reference it. They are not guessed: "
            "a wrong prefix inventories nothing and reads as a clean manifest."
            % entry.get("id"))

    packaged = {}
    referenced = set()
    for name, read in _artefact_members(artefact_path):
        if name.startswith(static_prefix):
            url = "/" + name[len(static_prefix):]
            packaged[url] = hashlib.sha256(read()).hexdigest()
        elif name.startswith(tpl_prefix):
            text = read().decode("utf-8", "replace")
            for url in _ASSET_REF_RE.findall(_HTML_COMMENT_RE.sub("", text)):
                if url.lower().endswith(_ASSET_EXT):
                    referenced.add(url)

    if not packaged:
        raise SystemExit(
            "entry %s inventoried ZERO asset under %r in %s. An empty inventory "
            "canonicalises to a manifest that is stable, green and blind, so it "
            "is refused: either the prefix is wrong, or the build packaged "
            "nothing." % (entry.get("id"), static_prefix, artefact_path))

    records = []
    for url in sorted(set(packaged) | referenced):
        # Le chemin est encodé pour la requête, jamais pour la référence. Un nom
        # de fichier portant une espace ou un accent, envoyé brut, fait une
        # requête malformée et une erreur de transport, que le manifeste aurait
        # consignée comme « ressource absente ». Un défaut du filet aurait été
        # rapporté comme un défaut du produit. Vu sur quatre documents.
        #
        # `follow=False` : une redirection n'est PAS une livraison. Suivie, un
        # refus d'autorisation rend 200 et le corps de la page de connexion, que
        # le manifeste consigne alors comme « la ressource est servie, avec
        # d'autres octets que ceux empaquetés » — un défaut de routage
        # imaginaire, là où le fait réel est que la ressource n'est pas
        # publique. Même quatre documents, deuxième déguisement.
        status, _headers, body = session.fetch(
            "GET", urllib.parse.quote(url, safe="/"), follow=False)
        records.append({
            "url": url,
            "status": status,
            "served_sha": hashlib.sha256(body or b"").hexdigest(),
            "served_len": len(body or b""),
            "packaged_sha": packaged.get(url),
            "referenced": url in referenced,
        })
    return json.dumps({"assets": records}, sort_keys=True).encode()
class Browser:
    """A headless browser, started once per capture and stopped after it.

    One per capture rather than one per entry: a browser costs about a second
    to start, and paying that per entry pushes whoever writes the corpus to
    audit fewer pages — a coverage decision taken for a performance reason,
    which is how audit surfaces quietly shrink.
    """

    def __init__(self, config, gm_dir):
        self.binary = config.get("browser_binary", "chromium")
        self.gm_dir = gm_dir
        self.proc = None
        self.port = None

    def start(self):
        if self.proc is not None:
            return
        if not shutil.which(self.binary):
            raise SystemExit(
                "an entry declares surface 'a11y' but %r is not on PATH. The lane "
                "renders pages in a real browser — an audit run against raw HTML "
                "measures the markup, not what a user is served." % self.binary)
        import socket
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        self.port = s.getsockname()[1]
        s.close()
        workdir = tempfile.mkdtemp(prefix="gm-browser-")
        profile = os.path.join(workdir, "profile")
        env = self._font_env(workdir)
        # La sortie du navigateur est CONSERVEE, pas jetee. Elle partait dans
        # /dev/null : quand le processus mourait, la seule chose qui savait
        # pourquoi etait precisement ce qu'on effacait, et l'echec se presentait
        # comme une page lente. Trois hypotheses ont ete depensees sur ce silence.
        self.log = os.path.join(workdir, "browser.log")
        self._logf = open(self.log, "wb")
        # `--disable-dev-shm-usage` : dans un conteneur, `/dev/shm` fait 64 Mo par
        # défaut, et le moteur de rendu s'y bloque au lieu d'échouer. Le symptôme
        # n'est pas un plantage mais une page qui ne finit jamais de charger —
        # l'événement `load` ne vient pas, et le plafond de chargement tombe en
        # accusant la lenteur de l'hôte. Mesuré : 240 s sur la première page en
        # CI, instantané sur la même page en local.
        #
        # Hypothèse dirigée par le symptôme, pas certitude : ce mode d'échec de
        # Chrome en conteneur est connu et correspond, mais il ne se reproduit
        # pas ici. Un passage vert le confirmera ou l'infirmera.
        self.proc = subprocess.Popen(
            [self.binary, "--headless", "--disable-gpu", "--no-sandbox",
             "--disable-dev-shm-usage", "--disable-software-rasterizer",
             # Chrome émet au démarrage des appels de service — variations,
             # mise à jour de composants, détection de changement de réseau. Sur
             # un réseau qui laisse pendre au lieu de refuser, ces appels ne
             # rendent jamais la main. Ils n'ont aucune utilité pour un audit.
             "--disable-background-networking", "--disable-component-update",
             "--disable-sync", "--disable-default-apps", "--disable-extensions",
             "--disable-client-side-phishing-detection", "--metrics-recording-only",
             "--no-first-run", "--no-default-browser-check", "--mute-audio",
             "--hide-scrollbars", "--force-device-scale-factor=1",
             "--remote-debugging-port=%d" % self.port,
             "--user-data-dir=" + profile, "about:blank"],
            stdout=self._logf, stderr=subprocess.STDOUT, env=env)
        deadline = time.time() + 60
        while time.time() < deadline:
            try:
                with urllib.request.urlopen(
                        "http://127.0.0.1:%d/json/version" % self.port, timeout=2):
                    return
            except Exception:
                if self.proc.poll() is not None:
                    raise SystemExit("%s exited before opening its debug port" % self.binary)
                time.sleep(0.5)
        raise SystemExit("%s never opened its debug port within 60s" % self.binary)

    def _font_env(self, workdir):
        """Un navigateur SANS POLICE ne rend pas mal : il meurt.

        Mesure, et le navigateur le dit lui-meme :
        `FATAL SkFontMgr_FontConfigInterface.cpp: Not implemented.` puis
        SIGABRT, precede de `glyph_count: 0` — fontconfig ne trouve aucune
        police, la voie de repli de Skia n'existe pas, et le processus avorte.
        En local ce sont les polices du systeme hote qui sauvent la mise ; dans
        un conteneur il n'y en a pas, et l'echec se presente comme une page qui
        ne charge jamais.

        C'est la meme famille de defaut que sur la lane binaire : un rendu qui
        depend des polices de la machine, invisible dans le diff. La reponse est
        la meme — declarer les polices au lieu d'esperer celles de l'hote.

        REFUS BRUYANT si aucun repertoire de polices n'existe. Un navigateur
        sans police produirait, au mieux, un audit d'une page sans texte :
        stable, plausible, et aveugle sur tout ce qui se lit.
        """
        # UNIQUEMENT les polices DECLAREES. Les repertoires du systeme hote sont
        # exclus exprès, et ce n'est pas un durcissement gratuit : avec eux, la
        # machine qui enregistre les references dispose de polices que la CI n'a
        # pas, le texte se rend autrement, et une violation de CONTRASTE
        # apparait ici et pas la-bas. Mesure : 11 noeuds `color-contrast` en
        # local contre 10 en integration continue, de facon reproductible des
        # deux cotes — une reference qui encode une propriete de son producteur,
        # exactement le defaut deja rencontre sur les polices des PDF.
        #
        # Le prix est une reference qui ne vaut que sous le jeu de polices
        # declare. C'est le but : ce jeu-la, lui, voyage.
        candidates = [
            os.path.join(os.environ.get("DEVBOX_PROFILE", ""), "share", "fonts"),
            os.path.join(os.getcwd(), ".devbox", "nix", "profile", "default", "share", "fonts"),
        ]
        dirs = [d for d in candidates if d and os.path.isdir(d)]
        if not dirs:
            raise SystemExit(
                "aucun repertoire de polices trouve pour le navigateur. Il n'en "
                "rendra pas moins bien : il AVORTE (SkFontMgr 'Not implemented'). "
                "`noto-fonts` et `fontconfig` sont declares dans devbox.json ; "
                "cherches ici : %s. Les polices du systeme hote ne sont "
                "DELIBEREMENT pas utilisees : une reference enregistree avec "
                "elles ne vaut que sur la machine qui les porte."
                % ", ".join(candidates))
        conf = os.path.join(workdir, "fonts.conf")
        with open(conf, "w", encoding="utf-8") as f:
            f.write("<?xml version='1.0'?>\n<fontconfig>\n")
            for d in dirs:
                f.write("  <dir>%s</dir>\n" % d)
            f.write("  <cachedir>%s</cachedir>\n" % os.path.join(workdir, "fc-cache"))
            f.write("</fontconfig>\n")
        env = dict(os.environ)
        env["FONTCONFIG_FILE"] = conf
        env["FONTCONFIG_PATH"] = workdir
        return env

    def why_it_died(self):
        """Ce que le navigateur a dit avant de partir, et son code de sortie.

        Sans ces deux faits, « socket fermee » est un constat sans cause : on
        sait que le processus est parti, pas ce qui l'a fait partir.
        """
        bits = []
        if self.proc is not None and self.proc.poll() is not None:
            bits.append("le navigateur s'est arrete (code %s)" % self.proc.returncode)
        try:
            with open(self.log, "rb") as f:
                tail = f.read()[-1200:].decode("utf-8", "replace").strip()
            if tail:
                bits.append("dernieres lignes du navigateur :\n" + tail)
        except (OSError, AttributeError):
            pass
        return " | ".join(bits) if bits else "le navigateur n'a rien ecrit"

    def stop(self):
        if getattr(self, "_logf", None) is not None:
            try:
                self._logf.close()
            except OSError:
                pass
            self._logf = None
        if self.proc is not None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=20)
            except subprocess.TimeoutExpired:
                self.proc.kill()
            self.proc = None

def collect_a11y(session, entry, browser):
    """Run the audit engine against the rendered page, as this persona sees it.

    The session's cookies are handed to the browser, so a page behind a login is
    audited as that role rather than as the login form. Without them the lane
    would report an audit of `/login` under every dashboard entry's name:
    stable, plausible, and about the wrong page.
    """
    browser.start()
    cookies = []
    for c in session.jar:
        cookies.append({"name": c.name, "value": c.value,
                        "domain": c.domain or "127.0.0.1",
                        "path": c.path or "/"})
    tmp = tempfile.mkdtemp(prefix="gm-a11y-")
    try:
        cookie_file = os.path.join(tmp, "cookies.json")
        with open(cookie_file, "w", encoding="utf-8") as f:
            json.dump(cookies, f)
        script = os.path.join(browser.gm_dir, "a11y", "run-axe.mjs")
        engine = os.path.join(browser.gm_dir, "a11y", "axe.min.js")
        for path in (script, engine):
            if not os.path.isfile(path):
                raise SystemExit(
                    "the a11y lane needs %s, which is missing. The audit engine is "
                    "vendored on purpose — see a11y/PROVENANCE.md — so the figures "
                    "can be recomputed rather than believed." % path)
        url = session.base_url + urllib.parse.quote(entry["path"], safe="/?&=%")
        p = subprocess.run(
            ["node", script, str(browser.port), url, engine, cookie_file],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=180)
        if p.returncode != 0:
            raise SystemExit("a11y audit of %s failed: %s\n%s"
                             % (entry["id"], (p.stderr or "").strip()[-800:],
                                browser.why_it_died()))
        return p.stdout.encode()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
def collect_canvas(session, entry, browser):
    """Ce qu'un canevas a réellement peint, sur la page telle que ce persona la voit.

    Cette lane couvre une surface qu'AUCUNE autre n'atteint. Le HTML servi
    contient une balise vide ; le DOM ne bouge plus une fois l'image peinte ;
    l'audit d'accessibilité le dit lui-même — une donnée rendue uniquement en
    couleur ou en canevas n'est pas restituée. Un graphique qui cesserait
    complètement de se dessiner laisserait donc toutes les autres références
    identiques à l'octet près.

    Même plomberie que la lane d'accessibilité, et pour la même raison : les
    cookies de la session sont remis au navigateur, sinon la lane mesurerait le
    canevas de la page de connexion sous le nom d'un écran d'administration.
    """
    browser.start()
    cookies = []
    for c in session.jar:
        cookies.append({"name": c.name, "value": c.value,
                        "domain": c.domain or "127.0.0.1",
                        "path": c.path or "/"})
    tmp = tempfile.mkdtemp(prefix="gm-canvas-")
    try:
        cookie_file = os.path.join(tmp, "cookies.json")
        with open(cookie_file, "w", encoding="utf-8") as f:
            json.dump(cookies, f)
        script = os.path.join(browser.gm_dir, "canvas", "run-canvas.mjs")
        shared = os.path.join(browser.gm_dir, "browser", "cdp.mjs")
        for path in (script, shared):
            if not os.path.isfile(path):
                raise SystemExit(
                    "the canvas lane needs %s, which is missing. It reads pixels "
                    "out of a real browser: without it the lane cannot exist, and "
                    "a lane that silently degraded to reading markup would report "
                    "a clean result about a surface it never looked at." % path)
        url = session.base_url + urllib.parse.quote(entry["path"], safe="/?&=%")
        load_timeout = int(os.environ.get("GM_A11Y_LOAD_TIMEOUT_S", "240"))
        # Le raster peut mettre jusqu'à vingt secondes à se stabiliser (voir
        # `run-canvas.mjs`), et cette attente SUIT le chargement. Le plafond du
        # sous-processus couvre les deux, sinon c'est lui qui tombe le premier et
        # le message parle d'un script tué au lieu d'une image qui bouge encore.
        p = subprocess.run(
            ["node", script, str(browser.port), url, cookie_file, str(load_timeout)],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            timeout=load_timeout + 180)
        if p.returncode != 0:
            raise SystemExit("canvas probe of %s failed: %s\n%s"
                             % (entry["id"], (p.stderr or "").strip()[-800:],
                                browser.why_it_died()))
        return p.stdout.encode()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def capture(config, corpus, canon, ids=None):
    """Fetch and canonicalise the corpus. Returns {id: canonical_text}."""
    sessions = open_sessions(config)
    jar_path = None
    browser = Browser(config, os.environ.get("GM_DIR", ".golden-master"))
    out = {}
    try:
        for e in corpus["entries"]:
            if ids is not None and e["id"] not in ids:
                continue
            s = sessions.get(e.get("persona", "anon"))
            if s is None:
                raise SystemExit("entry %s names unknown persona %r" % (e["id"], e.get("persona")))
            surface = e.get("surface")
            if surface == "asset":
                if jar_path is None:
                    jar_path = resolve_artifact(config, os.environ.get("GM_WORKSPACE", "."))
                status, headers, body = 200, {}, collect_assets(s, jar_path, e)
            elif surface == "a11y":
                status, headers, body = 200, {}, collect_a11y(s, e, browser)
            elif surface == "canvas":
                status, headers, body = 200, {}, collect_canvas(s, e, browser)
            else:
                status, headers, body = s.fetch(
                    e.get("method", "GET"), e["path"], fields=e.get("fields"),
                    follow=not e.get("no_redirect", False),
                )
            out[e["id"]] = canon.canonicalize(e, status, headers, body)
    finally:
        browser.stop()
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


_FP_SKIP = {".git", "node_modules", "build", ".gradle", ".gradle-ci", ".devbox",
            ".venv", "__pycache__", ".state"}


def tree_fingerprint(ws):
    """Empreinte de l'arbre, et JAMAIS `None` en cas d'echec.

    La version precedente rendait `None` quand git ne repondait pas — et
    `None != None` est faux, donc « l'arbre n'a pas bouge », donc « ce mutant ne
    change rien », donc INVALIDE. Un outil absent se presentait ainsi comme une
    propriete du mutant.

    Mesure de ce que ca coute : sur un runner ou git refusait de repondre
    (propriete du depot jugee douteuse par git, cas classique en conteneur),
    DIX mutants sur dix-neuf sont devenus invalides d'un coup. Et le score, qui
    se calcule sur les mutants VALIDES, est reste a 100 % — un contre-test
    reduit de moitie sous une note parfaite. C'est exactement le defaut que ce
    filet existe pour attraper, un cran au-dessus de lui.

    Le repli n'est pas un adoucissement : parcourir l'arbre donne une empreinte
    au moins aussi discriminante que `git status` pour ce qu'on lui demande —
    savoir si un fichier a bouge. Si les DEUX echouent, on s'arrete.
    """
    code, out = run("git status --porcelain", ws, timeout=120)
    if code == 0:
        return "git:" + out
    h = hashlib.sha256()
    seen = 0
    for root, dirs, files in os.walk(ws):
        dirs[:] = sorted(d for d in dirs if d not in _FP_SKIP)
        for name in sorted(files):
            full = os.path.join(root, name)
            try:
                st = os.stat(full)
            except OSError:
                continue
            h.update(("%s|%d|%d\n" % (os.path.relpath(full, ws), st.st_size,
                                      int(st.st_mtime))).encode())
            seen += 1
    if seen == 0:
        raise SystemExit(
            "impossible de prendre une empreinte de l'arbre dans %s : git n'a pas "
            "repondu et le parcours n'a vu aucun fichier. Sans elle, la validite "
            "d'un mutant n'est pas mesurable, et la faire passer pour « ce mutant "
            "ne change rien » accuserait le mutant a la place de l'outil." % ws)
    return "walk:" + h.hexdigest()




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


def diff_excerpt(left, right, ids, left_label, right_label, entries=4):
    """Les premieres lignes qui different, par entree.

    Nommer une entree sans montrer son ecart envoie chercher a l'aveugle — et
    quand l'instabilite ne se reproduit pas sur la machine qui lit le rapport,
    « 038 diverge » est une impasse. Quelques centaines d'octets remplacent une
    hypothese par un fait.

    Extrait ici plutot que recopie : le controle negatif l'avait, la sonde de
    stabilite ne l'avait pas, et rien ne justifiait l'asymetrie — c'est la sonde
    qui s'exprime en PREMIER quand les deux echouent, puisqu'elle interrompt le
    run avant que le controle negatif ne tourne.
    """
    detail = {}
    for i in list(ids)[:entries]:
        a = (left.get(i) or "").splitlines()
        b = (right.get(i) or "").splitlines()
        lines = []
        for k in range(max(len(a), len(b))):
            av = a[k] if k < len(a) else "<absente>"
            bv = b[k] if k < len(b) else "<absente>"
            if av != bv:
                lines.append("      %s -%s" % (left_label, av[:140]))
                lines.append("      %s +%s" % (right_label, bv[:140]))
            if len(lines) >= 8:
                break
        detail[i] = "\n".join(lines) or "(aucune ligne differente : longueurs seules)"
    return detail


# ─── Stability ──────────────────────────────────────────────────────────────

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
    # DETECTE PAR QUELLE SURFACE, et pas seulement « detecte ».
    #
    # Un mutant declare la surface qu'il vient SONDER. La detection, elle, etait
    # creditee des qu'une cible bougeait — sans regarder si cette entree relevait
    # de cette surface. Un jeu tenu a l'ecart tire CONTRE une lane pouvait donc
    # afficher 100 % pendant que la lane n'avait rien vu.
    #
    # Mesure du dixieme cycle : sept mutants tires contre la lane des canevas,
    # tous modifiant le meme fichier de script. Ce fichier est une ressource
    # servie, donc son empreinte bouge dans le MANIFESTE a chaque fois, quelle
    # que soit la consequence sur le produit. Le manifeste les a tous vus : 7/7.
    # La lane visee en a vu DEUX. Le chiffre publie etait vrai et trompeur.
    #
    # `blind_lanes` ne pouvait pas le dire : il ne parcourt que les mutants
    # visibles.
    surface_of = {e["id"]: e.get("surface", "http") for e in corpus["entries"]}
    verdict["moved_surfaces"] = sorted({surface_of.get(t, "?") for t in moved})
    declared_surface = meta.get("surface")
    verdict["detected_on_surface"] = bool(
        declared_surface and declared_surface in verdict["moved_surfaces"])
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
    # L'ETAT DE LA DONNEE, releve UNIQUEMENT quand le controle echoue.
    #
    # Le rapport nommait les entrees et montrait leur ecart, jamais ce que la
    # base contenait — et c'est le seul fait qui separe « le filet est
    # instable » de « l'etat a derive ». Sans lui, l'analyse d'un rouge qui ne se
    # reproduit pas se fait par elimination sur des journaux : le pipeline
    # `63384` a coute une session entiere pour une cause qui n'a jamais ete
    # etablie.
    #
    # La sonde est DECLAREE par la configuration, jamais devinee : le harnais ne
    # sait pas quelles colonnes ce produit rend, et une requete codee ici cesserait
    # d'etre universelle. Absente, le champ est vide et rien ne change.
    state = None
    if noisy and config.get("state_probe"):
        code, out = run(config["state_probe"], ws, timeout=120)
        state = out.strip() if code == 0 else "state_probe a echoue (%s):\n%s" % (code, out.strip()[:600])
    return {"silent": not noisy, "noisy_entries": noisy,
            "state_probe": state,
            "noisy_detail": diff_excerpt(refs, captured, noisy, "ref", "obs")}


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
              "holdout_detected_on_surface": 0, "score_on_surface_pct": 0,
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
            # Decoupe AVANT tout strip global. `git status --porcelain` ecrit
            # `XY <chemin>`, et X vaut ' ' pour une modification non indexee :
            # strip() sur la sortie entiere mange alors l'espace de tete de la
            # PREMIERE ligne seulement, qui se decale d'un cran et perd son
            # premier caractere (`build.gradle` -> `uild.gradle`). Les suivantes,
            # intactes, donnent une liste ou une seule entree est fausse — et
            # quand un seul fichier est sale, c'est la seule nommee.
            paths = [l[3:] for l in dirty.stdout.splitlines() if l.strip()]
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
                with open(os.path.join(refs_dir, k + ".txt"), "w", encoding="utf-8", newline="") as f:
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
            # `newline=""` DES DEUX COTES, et c'est la seule facon d'ecrire celui-ci :
            # l'ecriture par defaut ne traduit rien sous Linux et laisse partir les CR
            # bruts, la lecture par defaut active les universal newlines et remplace
            # `\r\n` par `\n`. Une reference dont le texte canonique porte un CR ne se
            # reproduit donc JAMAIS elle-meme, des l'instant ou elle est enregistree,
            # sans qu'aucun code n'ait bouge. Et le symptome ne nomme pas sa cause :
            # `splitlines()` traite les deux fins de ligne de la meme facon, donc la
            # comparaison ligne a ligne ne voit rien et publie sa phrase de repli —
            # « aucune ligne differente : longueurs seules ». C'est un ANGLE MORT :
            # toute reponse servie contenant un CR brut est invisible au filet.
            with open(p, encoding="utf-8", newline="") as f:
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
            # Le chiffre qui compte vraiment : combien ont ete vus PAR LA LANE
            # QU'ILS SONDENT. Toujours inferieur ou egal au precedent, et l'ecart
            # entre les deux est exactement ce qu'un « 100 % » cachait.
            holdout_detected_on_surface=len(
                [v for v in held if v.get("valid") and v.get("detected_on_surface")]),
            score_on_surface_pct=int(
                100 * len([v for v in valid if v.get("detected_on_surface")]) / len(valid)
            ) if valid else 0,
        )
        off = [v["id"] for v in held
               if v.get("valid") and v.get("detected") and not v.get("detected_on_surface")]
        if off:
            report["notice"] = (
                (report.get("notice") or "") +
                (" | " if report.get("notice") else "") +
                "%d held-out mutant(s) were detected by a lane OTHER than the one they "
                "probe: %s. The net saw the change; the lane they were drawn against did "
                "not. Citing the aggregate alone would report the resemblance."
                % (len(off), ", ".join(off)))
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
            detail = noop.get("noisy_detail") or {}
            problems.append(
                "NEGATIVE CONTROL FAILED: the no-op mutation made %s diverge. "
                "A comparator that fires without a change proves nothing when it "
                "fires with one.\n%s"
                % (noop["noisy_entries"][:8],
                   "\n".join("    %s :\n%s" % (i, detail[i]) for i in sorted(detail))))
            if noop.get("state_probe"):
                problems.append(
                    "ETAT DE LA DONNEE au moment de l'echec (sonde declaree par la "
                    "configuration) — c'est ce qui separe un filet instable d'un etat "
                    "qui a derive :\n%s" % noop["state_probe"])
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
