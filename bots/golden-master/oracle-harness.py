#!/usr/bin/env python3
"""Deterministic oracle harness — the bot-owned half of the golden-master bot.

Copy kept standalone for review. The executable copy is inlined in main.bot's
`oracle_run` tool node, and the two bodies are held BYTE-IDENTICAL by
`bots/golden_master_harness_sync_test.go` — edit either one and regenerate the
other. An obligation stated in prose is an obligation that drifts: these two
copies did, by sixty-two lines, while a test pinning only their function names
stayed green.

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
import stat
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
    # `canvas` WAS DECLARED REQUIRED IN THE SKILL AND ENFORCED NOWHERE. A net
    # carrying a canvas surface with no canvas mutant reported no gap and passed
    # — the lane existed, its counter-test did not have to. A requirement
    # written in one place and checked in none is the defect this harness exists
    # to catch, one level up.
    "canvas": ["content_empty", "render_drift"],
    # `write` is the surface no read-only capture can reach. Its archetypes are
    # required for the same reason the binary one is: without them the lane can
    # be present, green, and blind — a round trip that stores something other
    # than what it was given, or a creation that silently stopped persisting,
    # with every served byte identical.
    "write":  ["roundtrip_corruption", "create_lost"],
}

# Required corpus probes. Same enforcement doctrine as REQUIRED_ARCHETYPES: the
# skill explains each one, this constant binds the campaign. Every probe here
# exists because a real regression class lived in a corpus hole that no mutant
# could reach — a mutant tests the COMPARATOR on what the corpus watches; a
# probe forces the corpus to watch a class of surface teams systematically
# skip. An entry claims a probe through its `probes` list; where a shape can be
# checked mechanically, it is, so a tag without substance does not count.
#
#   write_create          a write entry that CREATES (INSERT), not only updates
#   error_then_corrected  a multi-step write: an invalid submission, then the
#                         corrected one — the round trip users actually make
#   case_pair             two read entries whose paths differ ONLY by case —
#                         collation and case-folding drift become visible
#   text_sort             a listing ordered on a TEXTUAL key — numeric-id sorts
#                         cannot see collation-order drift
#   auth_case             a persona whose login differs only by case from
#                         another persona's — identifier case-sensitivity drift
REQUIRED_CORPUS_PROBES = ("write_create", "error_then_corrected",
                          "case_pair", "text_sort", "auth_case")

CONTROL_SAMPLE = 6          # non-target entries replayed per mutant, for collateral
UP_LOG_LINES = 40           # lignes de `config.up` publiees dans le journal
BOOT_TIMEOUT_S = 300
CMD_TIMEOUT_S = 1800


def log(msg):
    print(msg, file=sys.stderr, flush=True)


def note(report, msg):
    """Ajoute une notice SANS effacer celles deja posees.

    Le champ etait ecrit par affectation a trois endroits et par concatenation a
    deux autres. Consequence mesuree : l'avertissement « arbre non committe » —
    celui qui previent que les reverts de mutants vont DETRUIRE le travail en
    cours — etait ecrase par la notice « jeu held-out depense », posee plus tard
    et par affectation. Comme le jeu est depense entre deux cycles, c'est-a-dire
    la plupart du temps, l'avertissement le plus couteux a manquer etait celui
    qui disparaissait le plus souvent.

    La cause etait calculee, puis supprimee avant publication.
    """
    report["notice"] = ((report["notice"] + " | ") if report["notice"] else "") + msg


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
        # Un chemin de configuration est DEVELOPPE avant d'etre resolu. L'etat
        # mutable d'un depot cible (datadir, socket, port) est declare une fois,
        # chez lui, et se surcharge par variable d'environnement pour que deux
        # jobs concurrents ne le partagent pas. Un chemin relatif fige ici serait
        # une SECONDE declaration de la meme chose — et deux declarations de la
        # meme chose derivent. Mesure : l'etat deplace par variable a laisse ces
        # chemins pointer sur l'ancien emplacement ; d'un cote le fichier n'y
        # etait pas et la porte l'a dit, de l'autre il y etait ENCORE, perime,
        # laisse par un job precedent sur le meme runner — et la porte a tourne
        # sur une URL qu'elle croyait fraiche. Le second cas est le dangereux :
        # il ne s'annonce pas.
        rel = os.path.expandvars(rel)
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
            # A case-variant persona is ALLOWED to be refused: whether the
            # application folds identifier case is precisely the behaviour the
            # `auth_case` probe pins, and the refusal is the reference then.
            # For every other persona a failed login stays fatal — a capture
            # taken with a broken session records the login page as every
            # reference.
            if code >= 400 and not p.get("case_variant_of"):
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
    rel = os.path.expandvars(rel)   # meme raison que dans resolve_base_url
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


def restore_world(config, ws):
    """Puts the seed back after an entry that WRITES.

    A write lane without a restore is not a write lane, it is a contamination:
    the first entry that posts leaves the world in another state, and EVERY
    entry captured afterwards describes something the fixture never seeded —
    silently, since each of them stays stable from one pass to the next.

    No quiet recovery. If the restore fails the capture stops: a net that
    carries on over a world it can no longer name returns a verdict about
    nothing.
    """
    cmd = config.get("restore")
    if not cmd:
        raise SystemExit(
            "the corpus carries an entry of surface 'write' and the configuration "
            "declares no `restore`. A write without a restore contaminates every "
            "entry captured after it; the harness refuses to capture rather than "
            "return a verdict about a world nobody seeded.")
    code, out = run(cmd, ws, timeout=BOOT_TIMEOUT_S)
    if code != 0:
        raise SystemExit("config.restore failed (exit %s):\n%s" % (code, out[-2000:]))


def capture(config, corpus, canon, ids=None):
    """Fetch and canonicalise the corpus. Returns {id: canonical_text}."""
    sessions = open_sessions(config)
    jar_path = None
    browser = Browser(config, os.environ.get("GM_DIR", ".golden-master"))
    out = {}
    ws = os.environ.get("GM_WORKSPACE", ".")
    # WRITES LAST, and the order is not a convenience: an entry that writes
    # changes the world the others observe. Capturing them after everything
    # else means a failed restore cannot silently contaminate the read-only
    # corpus — it stops the capture, and what was already captured was
    # captured on the seed.
    entries = ([e for e in corpus["entries"] if e.get("surface") != "write"]
               + [e for e in corpus["entries"] if e.get("surface") == "write"])
    try:
        for e in entries:
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
            elif surface == "write":
                # THE ROUND TRIP, and it is the whole point of this surface.
                # Every other entry watches a response SERVED. A corruption
                # that happens when content is STORED — a tag lost, an
                # attribute normalised, an id drawn at random — moves no
                # reference and passes the gate green. Measuring it takes a
                # script outside the net.
                #
                # A write entry therefore captures TWO things: what the write
                # answers, and what the readback renders. The second is the one
                # that counts — that is where content comes back deformed.
                # The token comes off the FORM. A security major can turn CSRF
                # on for state-changing requests where it was off; without it
                # the entry captures a refusal and yields a reference that can
                # never fail again — the very defect family this net hunts.
                tok_field = e.get("csrf_field")
                tok = None
                if tok_field:
                    _, _, form_body = s.fetch("GET", e["path"])
                    tok = extract_input_value(form_body, tok_field)

                def write_once(method, path, fields):
                    f = dict(fields or {})
                    if tok_field and tok:
                        f[tok_field] = tok
                    return s.fetch(method, path, fields=f,
                                   follow=not e.get("no_redirect", False))

                steps = e.get("steps") or []
                trail = []
                if steps:
                    # A SEQUENCE, captured whole, in one session. The probe
                    # this exists for is `error_then_corrected`: submit an
                    # invalid form, then submit its correction. What must not
                    # regress is the JOURNEY — an error state that sticks to
                    # the re-rendered form turns every later submission into a
                    # refusal, and a single-shot write can never see it.
                    for st in steps:
                        st_status, st_headers, st_body = write_once(
                            st.get("method", e.get("method", "POST")),
                            st.get("path", e["path"]), st.get("fields"))
                        trail.append((st_status, st_body))
                    w_status, w_headers = st_status, st_headers
                else:
                    w_status, w_headers, _ = write_once(
                        e.get("method", "POST"), e["path"], e.get("fields"))
                rb = e.get("readback")
                if not rb:
                    raise SystemExit(
                        "entry %s has surface 'write' and no `readback`: a write "
                        "nothing is read back from establishes the status code, "
                        "which is close to nothing" % e["id"])
                r_status, r_headers, r_body = s.fetch("GET", rb)
                status, headers = w_status, w_headers
                body = ((trail, (r_status, r_headers, r_body)) if steps
                        else (r_status, r_headers, r_body))
            else:
                status, headers, body = s.fetch(
                    e.get("method", "GET"), e["path"], fields=e.get("fields"),
                    follow=not e.get("no_redirect", False),
                )
            out[e["id"]] = canon.canonicalize(e, status, headers, body)
            if surface == "write":
                restore_world(config, ws)
    finally:
        browser.stop()
    return out


# ─── Application lifecycle ──────────────────────────────────────────────────

def app_up(config, ws):
    code, out = run(config["up"], ws, timeout=BOOT_TIMEOUT_S)
    if code != 0:
        raise SystemExit("config.up failed (exit %s):\n%s" % (code, out[-2000:]))
    # Ce que `config.up` a dit de lui-meme, PUBLIE — pas seulement garde en cas
    # d'echec.
    #
    # La sortie etait capturee et jetee des lors que la commande reussissait. Un
    # journal de CI ne portait donc AUCUNE trace de l'environnement que le filet
    # venait de mesurer : ni les versions, ni les ports, ni l'etat des donnees,
    # ni les services annexes demarres. Mesure : un referentiel geographique
    # local a ete ajoute au demarrage, et le fait qu'il ait servi ne pouvait se
    # DEDUIRE que du vert de la porte — un raisonnement, pas une trace, ce que
    # ce depot refuse partout ailleurs.
    #
    # Publie tel quel plutot que filtre : quelles lignes comptent est une
    # propriete du depot cible, pas du harnais, et un filtre ecrit ici finirait
    # par masquer l'annonce d'un service que personne n'a pense a lister.
    lines = [l for l in out.splitlines() if l.strip()]
    shown = lines[-UP_LOG_LINES:]
    if len(lines) > UP_LOG_LINES:
        log("config.up — %d dernieres lignes sur %d :" % (len(shown), len(lines)))
    else:
        log("config.up :")
    for l in shown:
        log("  | " + l)
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


def missing_corpus_probes(corpus, config):
    """Required corpus probes absent or claimed without substance.

    A probe is CLAIMED by an entry's `probes` list and COUNTS only when its
    mechanical shape holds — a tag alone is a declaration, and declarations are
    exactly what this harness refuses to grade. Checked before the application
    is booted, like the archetypes: a corpus with a hole in it cannot buy the
    figure back at capture time.
    """
    entries = corpus["entries"]
    tagged = {}
    for e in entries:
        for p in e.get("probes", []):
            tagged.setdefault(p, []).append(e)
    gaps = []

    ok_create = any(e.get("surface") == "write" and e.get("method", "POST") != "GET"
                    for e in tagged.get("write_create", []))
    if not ok_create:
        gaps.append({"probe": "write_create",
                     "why": "no write entry creates anything — the net only ever "
                            "updates, and a broken creation path moves no reference"})

    ok_seq = any(e.get("surface") == "write" and len(e.get("steps") or []) >= 2
                 for e in tagged.get("error_then_corrected", []))
    if not ok_seq:
        gaps.append({"probe": "error_then_corrected",
                     "why": "no multi-step write replays an invalid submission "
                            "followed by its correction (needs `steps`, >= 2)"})

    pair = [e.get("path", "") for e in tagged.get("case_pair", [])]
    ok_pair = any(a.lower() == b.lower() and a != b
                  for i, a in enumerate(pair) for b in pair[i + 1:])
    if not ok_pair:
        gaps.append({"probe": "case_pair",
                     "why": "no two entries differ only by case in their path — "
                            "case-folding and collation drift stay invisible"})

    ok_sort = any("?" in e.get("path", "") for e in tagged.get("text_sort", []))
    if not ok_sort:
        gaps.append({"probe": "text_sort",
                     "why": "no entry orders a listing on a textual key (tag one "
                            "whose query string carries the sort)"})

    personas = {p.get("name"): p for p in config.get("personas", [])}
    ok_auth = False
    for p in personas.values():
        ref = personas.get(p.get("case_variant_of"))
        if not ref:
            continue
        mine = (p.get("login") or {}).get("fields") or {}
        theirs = (ref.get("login") or {}).get("fields") or {}
        low = lambda d: {k: str(v).lower() for k, v in d.items()}
        observed = any(e.get("persona") == p.get("name") for e in entries)
        if mine and low(mine) == low(theirs) and mine != theirs and observed:
            ok_auth = True
    if not ok_auth:
        gaps.append({"probe": "auth_case",
                     "why": "no corpus entry observes a persona whose login is a "
                            "case variant of another persona's (declare "
                            "`case_variant_of` and use the persona in an entry)"})

    return gaps


def pending_rebaselines(gm_dir):
    """Ledger requests no act has answered — measured, four of them, resting
    quietly behind a green gate.

    A pending request means a known divergence is being carried: the entries it
    names are quarantined out of the verdict, and the gate is green AROUND
    them. Left unchecked, that state is exactly the failure this whole file
    exists to catch one level down — the net narrowing while every run reports
    green. So a pending request is a CONJUNCTION TERM: the gate refuses until
    the owner acts it (or a later request declares `\"replaces\"` and is acted
    itself; the supersedence must be the machine-readable field, prose does not
    count).

    A ledger that cannot be parsed is an escalation, never a silence — but an
    ABSENT ledger is a net that never re-baselined, and that is a legal state.
    """
    path = os.path.join(gm_dir, "REBASELINE.md")
    if not os.path.isfile(path):
        return []
    with open(path, encoding="utf-8") as f:
        text = f.read()

    def blocks(kind):
        out = []
        for body in re.findall(r"<!-- iterion:rebaseline-%s\n(.*?)\n-->" % kind,
                               text, re.S):
            try:
                obj = json.loads(body)
            except ValueError:
                obj = None
            if not isinstance(obj, dict) or \
                    not (isinstance(obj.get("id"), str) and obj.get("id")):
                obj = {"id": "UNPARSEABLE", "raw": body[:120]}
            out.append(obj)
        return out

    requests = blocks("request")
    acted = {b.get("id") for b in blocks("act")}
    if any(r.get("id") == "UNPARSEABLE" for r in requests) or "UNPARSEABLE" in acted:
        return [{"id": "UNPARSEABLE",
                 "why": "a ledger block does not parse as JSON — escalate, do not guess"}]

    replaced_by = {}
    for r in requests:
        if r.get("replaces"):
            replaced_by[r["replaces"]] = r["id"]

    def closed(rid, seen=()):
        if rid in acted:
            return True
        nxt = replaced_by.get(rid)
        if not nxt or nxt in seen:
            return False
        return closed(nxt, seen + (rid,))

    return [{"id": r["id"], "lot": r.get("lot", "?")}
            for r in requests if not closed(r["id"])]


# ─── Net extension — additions applied by the net's own subbot ──────────────
#
# The additive counterpart of the re-baseline ledger, with the opposite
# authority: a re-baseline MOVES a reference and only a human act closes it;
# an extension ADDS an observation point and the net's own subbot may act it,
# because an addition is checkable — it cannot mask an existing divergence,
# it can only add a constraint. The line that keeps that true is drawn here,
# mechanically: a delete or a rewrite wearing an addition's name is refused
# (removing the inconvenient reference and re-adding a "fresh" one that
# matches the broken behaviour IS the masking vector), and so is an addition
# whose observation tuple collides with an existing entry (two references for
# one observation resolve later by a "cleanup" that picks the masking
# direction).

def _extension_ledger_text(gm_dir):
    path = os.path.join(gm_dir, "EXTENSIONS.md")
    if not os.path.isfile(path):
        return ""
    with open(path, encoding="utf-8") as f:
        return f.read()


def _extension_blocks(text, kind):
    """Same block idiom as the re-baseline ledger: HTML comment, one JSON
    object. An unreadable block is an escalation, never a guess."""
    out = []
    for body in re.findall(r"<!-- iterion:extension-%s\n(.*?)\n-->" % kind,
                           text, re.S):
        try:
            obj = json.loads(body)
        except ValueError:
            obj = None
        if not isinstance(obj, dict) or \
                not (isinstance(obj.get("id"), str) and obj.get("id")):
            obj = {"id": "UNPARSEABLE", "raw": body[:120]}
        else:
            # The list-shaped fields are iterated by every judgement below. A
            # scalar here raised TypeError inside the verdict, and a verdict
            # that cannot answer was read as "nothing to refuse" by the lot
            # gate — the ledger disabling its own guard (adversarial finding,
            # executed). Normalising at the single parse point makes such a
            # block refusable instead of fatal.
            for field in ("recorded_paths", "paths", "corpus_entries",
                          "expected_paths", "entries"):
                if field in obj and not isinstance(obj[field], list):
                    obj[field] = []
            # Two spellings of a request are read, at this single parse
            # point, so every judgement below sees one shape. The canonical
            # one (`paths`, `corpus_entries` with full entries) is what the
            # doctrine teaches; the other (`expected_paths`, `entries` as
            # ids) is the re-baseline ledger's, and a ledger header written
            # by a worker in that idiom taught it to every request after it
            # — measured: every conforming request of a programme was judged
            # "smuggled", because the judge read only the first spelling.
            if kind == "request":
                if "paths" not in obj and isinstance(obj.get("expected_paths"), list):
                    obj["paths"] = [p for p in obj["expected_paths"] if isinstance(p, str)]
                if "corpus_entries" not in obj and isinstance(obj.get("entries"), list):
                    obj["corpus_entries"] = [
                        e if isinstance(e, dict) else {"id": e}
                        for e in obj["entries"] if isinstance(e, (dict, str))]
        out.append(obj)
    return out


def _records_extension_surface(paths):
    """An act closes a request only by recording the SURFACE an extension can
    reach: a reference, or the corpus. An act naming only the ledger records
    the paperwork of its own filing — it closed the conjunction term while the
    reference it declared did not exist (adversarial finding, executed)."""
    if not isinstance(paths, list):
        return False
    for p in paths:
        if not isinstance(p, str) or not p:
            continue
        rel = p.split("/", 1)[1] if p.startswith(".golden-master/") else p
        if rel.startswith("refs/") or rel == "corpus.json":
            return True
    return False


def pending_extensions(gm_dir, text=None):
    """Extension requests no act has answered — a conjunction term, like
    pending re-baselines, for the mirrored reason: a pending request names an
    observation the net was ASKED to gain and does not have, so a green built
    while it waits reports coverage its own intent knows is missing. There is
    no `replaces` chain here: an extension that no longer applies is acted or
    withdrawn by its requester, not superseded."""
    if text is None:
        text = _extension_ledger_text(gm_dir)
    if not text:
        return []
    requests = _extension_blocks(text, "request")
    acts = _extension_blocks(text, "act")
    if any(b.get("id") == "UNPARSEABLE" for b in requests + acts):
        return [{"id": "UNPARSEABLE",
                 "why": "a ledger block does not parse as JSON — escalate, do not guess"}]
    # Only a WELL-FORMED act closes a request: an act with no recorded path
    # acted nothing, and letting it close the term would let the constrained
    # party silence the conjunction with four lines of JSON — the
    # additions-only verdict never runs at gate time, so this shape check is
    # the whole defence here (adversarial finding, executed).
    acted = {b["id"] for b in acts
             if _records_extension_surface(b.get("recorded_paths"))}
    return [{"id": r["id"], "lot": r.get("lot", "?")}
            for r in requests if r["id"] not in acted]


## The fields that DECIDE what an entry observes. An allowlist, not
## "everything but id": a stripped-everything key is defeated by one cosmetic
## key (`note`, `comment`) that makes two identical observations compare
## different (adversarial finding, executed). Fields outside this list do not
## discriminate; if the harness later grows a discriminating field, the
## failure mode is a FALSE collision — refused, escalated to the requester —
## never a masked duplicate.
OBSERVATION_FIELDS = ("method", "path", "persona", "surface", "fields",
                      "steps", "readback", "no_redirect", "csrf_field",
                      "static_prefix", "template_prefix", "probes")


def _entry_observation_key(entry):
    """What an entry OBSERVES: two entries equal under this key are two
    references for one observation — a collision, not an addition. Absent
    and empty collapse together — a twin carrying `"query": ""` must not
    split the key on mere PRESENCE (consolidation finding, executed)."""
    return json.dumps({k: (entry.get(k) or None) for k in OBSERVATION_FIELDS},
                      sort_keys=True, ensure_ascii=False)


def extension_verdict(ws, gm_rel, base):
    """Judge every ACTED extension against `base`, in git — never against the
    acting party's word.

    Per acted request, every recorded path must be a pure addition:
      - under refs/: absent at base, present at HEAD. A path that existed at
        base is a rewrite wearing an addition's name; one absent at HEAD is a
        delete — both are the masking vector and both refuse. A rename is a
        delete plus a fresh file, judged separately, and the delete side
        loses.
      - corpus.json: every base entry survives equal, non-entry keys
        untouched, every added entry is claimed by an acted request's
        `corpus_entries`, and no addition collides — with the base corpus or
        with a sibling addition — on its observation tuple.
    The ledger itself must be append-only (base text is a prefix of HEAD
    text): history in the ledger is the audit trail, and an edited trail
    audits nothing.
    """
    def git(*args):
        p = subprocess.run(["git", "-C", ws] + list(args),
                           capture_output=True, text=True, timeout=120)
        return p.returncode, p.stdout, p.stderr

    def at(ref, path):
        code, out, _ = git("show", "%s:%s" % (ref, path))
        return out if code == 0 else None

    def blob_mode(ref, path):
        code, out, _ = git("ls-tree", ref, "--", path)
        return out.split()[0] if code == 0 and out.strip() else ""

    ledger_rel = gm_rel.rstrip("/") + "/EXTENSIONS.md"
    corpus_rel = gm_rel.rstrip("/") + "/corpus.json"
    refs_prefix = gm_rel.rstrip("/") + "/refs/"

    def introducing_commits():
        """First commit in which each ledger block id appears, oldest first.

        Separation of powers is a claim about WHO wrote what, and the
        ledger's own history is the only record of it neither party writes
        twice. A request and its act sharing an introducing commit means the
        constrained party filed and answered in one gesture, so the net's
        subbot never ran — production cannot produce that shape, because the
        parent commits its lot before the subbot starts (adversarial finding,
        executed)."""
        code, out, _ = git("log", "--format=%H", "--reverse", "--", ledger_rel)
        if code != 0:
            return None
        first = {}
        for sha in out.split():
            text = at(sha, ledger_rel)
            if text is None:
                continue
            for kind in ("request", "act"):
                for b in _extension_blocks(text, kind):
                    bid = b.get("id")
                    if isinstance(bid, str) and bid:
                        first.setdefault((kind, bid), sha)
        return first

    # An unresolvable base and an honest "absent at base" both read as None
    # below, so a bad sha would certify everything as an addition — a guard
    # whose failure mode is a full pass (adversarial finding, executed).
    # Refuse it the way an empty base is refused.
    code, _, err = git("rev-parse", "--verify", "--quiet", base + "^{commit}")
    if code != 0:
        return {"error": "GM_BASE %r does not resolve to a commit (%s) — "
                         "judging additions against nothing would pass by "
                         "construction" % (base, err.strip() or "unknown ref")}

    verdict = {"acted": [], "ok_paths": [], "ledger_append_only": True,
               "requests_added": 0, "problems": []}

    head_txt = at("HEAD", ledger_rel) or ""
    base_txt = at(base, ledger_rel) or ""
    if base_txt and not head_txt.startswith(base_txt):
        verdict["ledger_append_only"] = False
        verdict["problems"].append(
            "EXTENSIONS.md was REWRITTEN, not appended — history in the "
            "ledger is the audit trail, and an edited trail audits nothing")

    all_blocks = (_extension_blocks(head_txt, "request") +
                  _extension_blocks(head_txt, "act"))
    if any(b.get("id") == "UNPARSEABLE" for b in all_blocks):
        verdict["problems"].append(
            "a ledger block does not parse as JSON — escalate, do not guess")
    requests = {b["id"]: b for b in _extension_blocks(head_txt, "request")
                if b.get("id") != "UNPARSEABLE"}
    acts = [b for b in _extension_blocks(head_txt, "act")
            if b.get("id") != "UNPARSEABLE"]
    base_req_ids = {b.get("id") for b in _extension_blocks(base_txt, "request")}
    verdict["requests_added"] = len(set(requests) - base_req_ids)

    # The corpus is judged ONCE, globally: additions-only, every base entry
    # intact, every addition claimed by an acted request, no collision.
    corpus_ok, corpus_problems, added_ids = True, [], set()
    head_corpus_txt = at("HEAD", corpus_rel)
    base_corpus_txt = at(base, corpus_rel)
    corpus_changed = head_corpus_txt != base_corpus_txt
    if corpus_changed:
        head_c = base_c = None
        try:
            head_c = json.loads(head_corpus_txt or "{}")
            base_c = json.loads(base_corpus_txt or "{}")
        except ValueError as e:
            corpus_problems.append("corpus.json does not parse: %s" % e)
        if head_c is not None and base_c is not None and \
                not isinstance(head_c, dict):
            corpus_problems.append("corpus.json is not an object")
            head_c = base_c = None
        if head_c is not None and base_c is not None and (
                not isinstance(head_c.get("entries", []), list)
                or not all(isinstance(e, dict)
                           for e in head_c.get("entries", []))):
            corpus_problems.append(
                "corpus.json `entries` is not a list of objects — refused, "
                "not crashed")
            head_c = base_c = None
        if head_c is not None and base_c is not None:
            if {k: v for k, v in head_c.items() if k != "entries"} != \
                    {k: v for k, v in base_c.items() if k != "entries"}:
                corpus_problems.append(
                    "corpus.json keys outside `entries` changed — an "
                    "extension adds entries and touches nothing else")
            # An id names ONE observation. Duplicated ids collapse in every
            # by-id index (this one, the capture map, the refs map), so the
            # equality check would read the surviving twin while the capture
            # hands the reference to the other — an existing observation
            # hijacked through the channel (adversarial finding, executed).
            head_ids = [e.get("id") for e in head_c.get("entries", [])]
            dupes = sorted({i for i in head_ids if head_ids.count(i) > 1})
            if dupes:
                corpus_problems.append(
                    "duplicate entry id(s) %s — an id is one observation, "
                    "and a twin id hands the capture to whichever entry "
                    "wins an ordering nobody audits" % ", ".join(map(repr, dupes)))
            base_by_id = {e.get("id"): e for e in base_c.get("entries", [])
                          if isinstance(e, dict)}
            head_by_id = {e.get("id"): e for e in head_c.get("entries", [])}
            for bid, be in base_by_id.items():
                if bid not in head_by_id:
                    corpus_problems.append(
                        "entry %r was REMOVED — a delete is the masking "
                        "vector, not an extension" % bid)
                elif head_by_id[bid] != be:
                    corpus_problems.append(
                        "entry %r was MODIFIED — a rewrite is the masking "
                        "vector, not an extension" % bid)
            added_ids = set(head_by_id) - set(base_by_id)
            # An entry id DERIVES a reference path (refs/<id>.txt) in the
            # record and gate paths, unvalidated there — so an added id
            # carrying a separator is a write to an EXISTING reference
            # wearing an addition's name, the corpus-surface twin of the
            # refs/ symlink (consolidation finding, executed with
            # id "../refs/1").
            for aid in sorted(added_ids):
                if not isinstance(aid, str) or not aid or aid in (".", "..") \
                        or any(c in aid for c in "/\\") \
                        or aid != os.path.basename(aid):
                    corpus_problems.append(
                        "added entry id %r derives a reference path outside "
                        "refs/ — a write to an existing reference wearing an "
                        "addition's name" % (aid,))
            claimed = set()
            for act in acts:
                req = requests.get(act.get("id"))
                for e in (req or {}).get("corpus_entries") or []:
                    if isinstance(e, dict) and e.get("id"):
                        claimed.add(e["id"])
            unclaimed = added_ids - claimed
            if unclaimed:
                corpus_problems.append(
                    "%d added entr%s no acted request claims: %s — an "
                    "addition smuggled beside an acted one is still smuggled"
                    % (len(unclaimed), "y" if len(unclaimed) == 1 else "ies",
                       ", ".join(sorted(unclaimed))))
            base_keys = {_entry_observation_key(e)
                         for e in base_by_id.values()}
            seen_new = {}
            for aid in sorted(added_ids):
                key = _entry_observation_key(head_by_id[aid])
                if key in base_keys or key in seen_new:
                    other = seen_new.get(key, "an existing entry")
                    corpus_problems.append(
                        "added entry %r observes the same tuple as %s — two "
                        "references for one observation resolve later by a "
                        "cleanup that picks the masking direction"
                        % (aid, other))
                seen_new[key] = "added entry %r" % aid
        corpus_ok = not corpus_problems

    # An act already recorded in the ledger AT BASE was judged by the run
    # that introduced it, and its additions are that base's references now:
    # judging it again against a base that contains them reads every one as
    # "existed at base — a rewrite wearing an addition's name" and refuses a
    # net that did nothing wrong. Measured on a live campaign: an extension
    # certified one day blocked every lot launched from the next day's base,
    # and the repair was a whole re-sealing rite — for a verdict that should
    # have said "already judged". Only acts the segment base..HEAD introduces
    # are judged here; the rest are reported acted-at-base and left alone.
    base_act_ids = {b.get("id") for b in _extension_blocks(base_txt, "act")
                    if b.get("id") != "UNPARSEABLE"}
    intro = introducing_commits()
    for act in acts:
        row = {"id": act.get("id"), "paths": [], "ok": False, "problems": []}
        if act.get("id") in base_act_ids:
            row["ok"] = True
            row["acted_at_base"] = True
            verdict["acted"].append(row)
            continue
        req = requests.get(act.get("id"))
        if req is None:
            row["problems"].append("an act without a request acts nothing")
        elif intro is None:
            row["problems"].append(
                "cannot read the ledger's history — provenance unproven, and "
                "an unprovable act is refused, not assumed")
        elif intro.get(("act", act.get("id"))) is not None \
                and intro.get(("act", act.get("id"))) == intro.get(("request", act.get("id"))):
            row["problems"].append(
                "the requester acted its own request: %s introduced both the "
                "request and the act, so the net's subbot never judged it — "
                "filing and answering are different powers"
                % intro[("act", act.get("id"))][:12])
        paths = [p for p in (act.get("recorded_paths") or [])
                 if isinstance(p, str) and p]
        row["paths"] = paths
        if req is not None and not paths:
            row["problems"].append(
                "the act records no path — an extension that touched "
                "nothing extended nothing")
        for p in paths:
            if p == ledger_rel:
                continue
            if p == corpus_rel:
                if not corpus_changed:
                    row["problems"].append(
                        "corpus.json is recorded but did not change")
                elif not corpus_ok:
                    row["problems"].extend(corpus_problems)
            elif p.startswith(refs_prefix):
                if at(base, p) is not None:
                    row["problems"].append(
                        "%s existed at base — a rewrite wearing an "
                        "addition's name" % p)
                elif at("HEAD", p) is None:
                    row["problems"].append(
                        "%s is recorded but absent at HEAD — a recorded "
                        "delete, and a delete is the masking vector" % p)
                elif blob_mode("HEAD", p) not in ("100644", "100755"):
                    # A symlink's content is its target string, so it passes
                    # every text check while the next record pass writes
                    # THROUGH it onto an existing reference — an addition
                    # that masks (adversarial finding, executed).
                    row["problems"].append(
                        "%s is not a regular file (mode %s) — a link is a "
                        "write to somewhere else wearing an addition's name"
                        % (p, blob_mode("HEAD", p) or "?"))
                elif req is not None and p not in (req.get("paths") or []) \
                        and p not in {refs_prefix + e["id"] + ".txt"
                                      for e in (req.get("corpus_entries") or [])
                                      if isinstance(e, dict)
                                      and isinstance(e.get("id"), str)
                                      and e["id"] == os.path.basename(e["id"])}:
                    # Added refs are claimed against the request — explicitly
                    # in `paths` (add-file), or implicitly as the reference an
                    # add-entry's claimed entry DERIVES (the gate demands
                    # refs/<id>.txt for every entry, so a claimed entry claims
                    # its reference; ids carrying separators derive nothing —
                    # they are refused above). Anything else is smuggling.
                    row["problems"].append(
                        "%s is neither declared in the request's `paths` nor "
                        "derived from a claimed corpus entry — acting more "
                        "than was asked is smuggling" % p)
            else:
                row["problems"].append(
                    "%s is outside the extension surface (refs/ and "
                    "corpus.json) — the canon, the mutants, the harness and "
                    "the configuration are the judge's territory" % p)
        if req is not None and not _records_extension_surface(paths):
            # The ledger is the paperwork of the filing, not the extension:
            # an act recording only it certified paths the net never gained
            # (adversarial finding, executed).
            row["problems"].append(
                "the act records no path on the extension surface (refs/ or "
                "corpus.json) — filing paperwork is not an extension")
        row["ok"] = (req is not None and not row["problems"]
                     and verdict["ledger_append_only"])
        if row["ok"]:
            verdict["ok_paths"].extend(row["paths"])
        verdict["acted"].append(row)

    # Corpus damage must surface even when NO act recorded corpus.json —
    # otherwise a hand-run of this mode shows every row ok and an empty
    # problems list while the corpus sits amputated, and the reader walks
    # away reassured by the very command meant to warn them (review note).
    if corpus_changed and corpus_problems and \
            not any(corpus_rel in (a.get("recorded_paths") or []) for a in acts):
        verdict["problems"].extend(corpus_problems)

    verdict["ok_paths"] = sorted(set(verdict["ok_paths"]))
    return verdict


# ─── Route coverage — the corpus states its own perimeter ───────────────────

def route_regex(pattern):
    """One route pattern -> anchored regex over the PATH part of an entry.

    `{x}` and `:x` match one segment, `*` one segment, `**` any tail. Nothing
    else is interpreted — in particular a trailing slash is NOT folded away:
    with-slash and without-slash are different routes, and the difference has
    shipped real 404s.
    """
    out = []
    segs = pattern.split("/")
    for i, seg in enumerate(segs):
        if seg == "**":
            if i != len(segs) - 1:
                raise SystemExit("route pattern %r uses `**` before the tail — "
                                 "the doctrine says a tail, and a mid-path "
                                 "`**` silently widens the perimeter" % pattern)
            out.append(".*")
        elif seg == "*" or (seg.startswith("{") and seg.endswith("}")) or seg.startswith(":"):
            out.append("[^/]+")
        else:
            out.append(re.escape(seg))
    return re.compile("^" + "/".join(out) + "$")


def route_gaps(routes, corpus, exclusions):
    """Routes the corpus does not cover and the exclusions do not justify.

    `routes` : [{"method": "GET"|None, "pattern": "/x/{id}"}]
    `exclusions` : {pattern | "METHOD pattern": reason} — validated by the
    caller (reason mandatory there; here they are simply honoured). Both key
    shapes count because the refusal message prints `METHOD pattern`: an
    exclusion transcribed verbatim from that message must be honoured, and a
    bare pattern excludes the route whatever its method.
    """
    paths = [(e.get("method", "GET").upper(), e.get("path", "").split("?")[0])
             for e in corpus["entries"] if e.get("path")]
    uncovered = []
    for r in routes:
        shown = (r["method"] + " " if r["method"] else "") + r["pattern"]
        if r["pattern"] in exclusions or shown in exclusions:
            continue
        rx = route_regex(r["pattern"])
        hit = any((r["method"] is None or r["method"] == m) and rx.match(p)
                  for m, p in paths)
        if not hit:
            uncovered.append(shown)
    return uncovered


def route_coverage(gm_dir, config, corpus, ws):
    """Runs the target's `routes_probe`, honours justified exclusions, returns
    (uncovered, total, excluded). Raises SystemExit with the cause on refusal.

    The probe is the TARGET's statement of its own surface — one route per
    line, `METHOD /path/{param}` or bare `/path`, `#` comments allowed. It is
    written by the campaign next to `state_probe`, because only the target
    knows its stack; the harness only refuses a net that cannot state the
    perimeter it claims to defend.
    """
    code, out = run(config["routes_probe"], ws, timeout=120)
    if code != 0:
        raise SystemExit("routes_probe failed (exit %s):\n%s" % (code, out[-1500:]))
    routes = []
    for line in out.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        head, _, tail = line.partition(" ")
        if tail and head.isalpha() and head.isupper():
            routes.append({"method": head, "pattern": tail.strip()})
        else:
            routes.append({"method": None, "pattern": line})
    if not routes:
        raise SystemExit("routes_probe printed no route — an empty perimeter is a "
                         "statement the corpus covers everything, and it is false "
                         "by construction")

    exclusions = {}
    excl_path = os.path.join(gm_dir, "route-coverage.json")
    if os.path.isfile(excl_path):
        with open(excl_path, encoding="utf-8") as f:
            declared = json.load(f).get("exclusions", [])
        for x in declared:
            if not (x.get("route") and (x.get("reason") or "").strip()):
                raise SystemExit(
                    "route-coverage.json carries an exclusion without a written "
                    "reason (%r). An exclusion is a decision; a decision has a "
                    "cause or it is a hole." % x)
            exclusions[x["route"]] = x["reason"]

    uncovered = route_gaps(routes, corpus, exclusions)
    return uncovered, len(routes), len(exclusions)


def parse_feature_probe(out):
    """The probe's statement -> {feature_id: sorted(sources)}.

    One feature per line, `<feature-id> <source>`, `#` comments allowed. The
    same feature printed by several sources is the point: a single source only
    shows what it shows. Refusals carry their cause — a line without a source
    is a statement without provenance, an empty statement claims the corpus
    covers every feature, and a single-source inventory has no independent
    witness.
    """
    feats = {}
    for line in out.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.split()
        if len(parts) != 2:
            raise SystemExit("feature_probe printed %r — expected `<feature-id> "
                             "<source>`, one per line" % line)
        fid, src = parts
        feats.setdefault(fid, set()).add(src)
    if not feats:
        raise SystemExit("feature_probe printed no feature — an empty inventory "
                         "is a statement the corpus covers every feature, and it "
                         "is false by construction")
    sources = set().union(*feats.values())
    if len(sources) < 2:
        raise SystemExit("feature_probe drew from a single source (%s): an "
                         "inventory needs at least two independent witnesses — "
                         "the served navigation graph and the tree's "
                         "message/template catalogue are the canonical pair — or "
                         "it only sees what that one source shows"
                         % ", ".join(sorted(sources)))
    return {f: sorted(s) for f, s in feats.items()}


def feature_gaps(features, coverage, corpus):
    """(unmapped, stale, broken) between the probe's live statement and the
    committed inventory.

    UNMAPPED: printed now, neither mapped to corpus entries nor excluded in
    writing. STALE: carried by the inventory but no longer printed — a map to
    a surface that is gone reads as coverage. BROKEN: a mapping citing a
    corpus id that does not exist — evidence that is not there.
    """
    ids = {e["id"] for e in corpus["entries"]}
    mapped = {m.get("feature"): m for m in coverage.get("features", [])}
    excluded = {x.get("feature") for x in coverage.get("exclusions", [])}
    unmapped = sorted(f for f in features if f not in mapped and f not in excluded)
    stale = sorted(f for f in (set(mapped) | excluded) if f not in features)
    broken = sorted("%s -> %s" % (f, i) for f, m in mapped.items()
                    for i in (m.get("entries") or []) if i not in ids)
    return unmapped, stale, broken


def validate_feature_coverage(coverage):
    """Shape-checks the committed inventory and returns it NORMALISED.

    Every refusal is named — a malformed inventory must never surface as a
    traceback: the report with its defaults would be lost and the cause with
    it. Normalisation matters as much as the checks: a `features: null` that
    merely passed validation would still crash the gap computation.
    """
    if not isinstance(coverage, dict):
        raise SystemExit("feature-coverage.json must be an object, got %s"
                         % type(coverage).__name__)
    for key in ("features", "exclusions"):
        block = coverage.get(key) or []
        if not isinstance(block, list) or any(not isinstance(x, dict) for x in block):
            raise SystemExit("feature-coverage.json `%s` must be a list of "
                             "objects — a malformed inventory is a named "
                             "refusal, never a traceback" % key)
        coverage[key] = block
    seen = set()
    for m in coverage["features"]:
        feat = m.get("feature")
        if not isinstance(feat, str) or not feat:
            raise SystemExit("feature-coverage.json maps a non-string feature "
                             "id (%r) — the probe prints strings, so this "
                             "mapping can never match and reads as coverage"
                             % (feat,))
        if feat in seen:
            raise SystemExit("feature-coverage.json maps %r twice — the second "
                             "mapping silently wins, and nobody chose which"
                             % feat)
        seen.add(feat)
        entries = m.get("entries")
        if entries is not None and not isinstance(entries, list):
            raise SystemExit("feature-coverage.json maps %r with `entries` that "
                             "is not a list (%r) — a string would be read one "
                             "character at a time" % (feat, entries))
        if not (entries or []):
            raise SystemExit(
                "feature-coverage.json maps %r to no corpus entry. A mapping to "
                "nothing is an exclusion wearing a map's clothes — move it to "
                "`exclusions` with its reason, or point it at real entries." % (m,))
    seen_excl = set()
    for x in coverage["exclusions"]:
        feat = x.get("feature")
        reason = x.get("reason")
        if not isinstance(feat, str) or not feat:
            raise SystemExit("feature-coverage.json excludes a non-string "
                             "feature id (%r) — it can never match the probe"
                             % (feat,))
        if feat in seen:
            raise SystemExit("feature-coverage.json both maps AND excludes %r — "
                             "two verdicts on one feature, and nobody chose"
                             % feat)
        if feat in seen_excl:
            raise SystemExit("feature-coverage.json excludes %r twice — two "
                             "reasons, and nobody chose which one stands"
                             % feat)
        seen_excl.add(feat)
        if not (isinstance(reason, str) and reason.strip()):
            raise SystemExit(
                "feature-coverage.json carries an exclusion without a written "
                "reason (%r). An exclusion is a decision; a decision has a "
                "cause or it is a hole." % (x,))
    return coverage


def standard_mark_verdict(gm_dir, std):
    """(level, message) for the standard ratchet — 'bail', 'note' or 'ok'.

    The mark file is the owner's committed memory of the standard reached;
    config.json is the live declaration. Divergence in either direction is a
    named refusal: a vanished `standard` field must not read as a legitimate
    standard-2 net, and a raise must move both declarations in one commit.
    """
    mark_path = os.path.join(gm_dir, "standard-mark")
    if os.path.isfile(mark_path):
        try:
            with open(mark_path, encoding="utf-8") as f:
                mark = int(f.read().strip())
        except (OSError, ValueError):
            return "bail", "standard-mark is unreadable — it must hold one integer"
        if std < mark:
            return "bail", ("config declares standard %d but standard-mark says "
                            "%d was reached: a standard only comes down through "
                            "the ledger, with its cause. Restore `standard` in "
                            "config.json, or record the downgrade as a net "
                            "change and lower the mark in the same commit."
                            % (std, mark))
        if std > mark:
            return "bail", ("config declares standard %d but standard-mark "
                            "still says %d: raise the mark in the SAME commit "
                            "as the config — the pair moving together is what "
                            "makes the ratchet auditable." % (std, mark))
        return "ok", ""
    if std >= 3:
        return "note", ("standard %d declared without a standard-mark file: "
                        "commit one (a single integer) so a later silent "
                        "downgrade cannot read as a legitimate standard-2 net."
                        % std)
    return "ok", ""


def feature_coverage(gm_dir, config, corpus, ws):
    """Runs the target's `feature_probe` against the LIVE application, honours
    the committed inventory, returns (unmapped, stale, total, excluded).
    Raises SystemExit with the cause on refusal.

    The probe is the target's statement of its own FEATURES — the level above
    routes: a route the corpus touches once is not a feature it exercises. It
    runs with the application up and `GM_BASE_URL` exported, so it can walk
    the served navigation AND read the tree's catalogues; only the campaign
    knows how. The inventory (`feature-coverage.json`) maps each feature to
    the corpus entries that exercise it, or excludes it in writing.
    """
    os.environ["GM_BASE_URL"] = resolve_base_url(config, ws)
    code, out = run(config["feature_probe"], ws, timeout=300)
    if code != 0:
        raise SystemExit("feature_probe failed (exit %s):\n%s" % (code, out[-1500:]))
    features = parse_feature_probe(out)

    coverage = {}
    cov_path = os.path.join(gm_dir, "feature-coverage.json")
    if os.path.isfile(cov_path):
        try:
            with open(cov_path, encoding="utf-8") as f:
                coverage = json.load(f)
        except ValueError as e:
            raise SystemExit("feature-coverage.json is not valid JSON: %s" % e)
    coverage = validate_feature_coverage(coverage)

    unmapped, stale, broken = feature_gaps(features, coverage, corpus)
    if broken:
        raise SystemExit(
            "feature-coverage.json cites corpus entries that do not exist: %s. "
            "The map points at evidence that is not there." % ", ".join(broken))
    return unmapped, stale, len(features), len(coverage.get("exclusions", []))


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
    # The default root is the system temp dir, NOT the workspace's parent: in a
    # sandboxed run the worktree is the only writable checkout path and its
    # parent is read-only — deriving a sibling there dies on the first seal.
    # The name hashes the ABSOLUTE path because sibling worktrees of one repo
    # share a basename; under a common root, basename alone would make two
    # campaigns seal into each other's pile.
    root = os.environ.get("GM_SCRATCH", tempfile.gettempdir())
    ap = os.path.abspath(ws)
    tag = hashlib.sha256(ap.encode("utf-8")).hexdigest()[:10]
    return os.path.join(root, "gm-holdout-%s-%s" % (os.path.basename(ap) or "default", tag))


def seal_committed_opted_in(gm_dir):
    """The convergence gate's opt-in to consume a COMMITTED held-out set.

    Two forms, either suffices: `"seal_committed": true` in config.json —
    written by the net's owner, committed, auditable, the preferred form —
    or GM_SEAL_COMMITTED=1 in the environment for a hand-run gate. The flag
    can only widen what the gate consumes, never soften a verdict, which is
    why an environment form is tolerable here at all.
    """
    env = os.environ.get("GM_SEAL_COMMITTED")
    if env == "1":
        return True
    if env not in (None, "", "0"):
        raise SystemExit("GM_SEAL_COMMITTED=%r is neither 1 nor 0 — a truthy "
                         "spelling silently ignored would leave the operator "
                         "sure of an opt-in that never happened" % env)
    try:
        with open(os.path.join(gm_dir, "config.json"), encoding="utf-8") as f:
            v = json.load(f).get("seal_committed", False)
    except (OSError, ValueError):
        return False
    if isinstance(v, bool):
        return v
    raise SystemExit("config `seal_committed` must be JSON true or false, got %r "
                     "— a mistyped opt-in silently ignored would leave the set "
                     "unconsumed with no one told" % (v,))


def holdout_committed_in_tree(gm_dir):
    """True when mutants/holdout/ carries COMMITTED entries.

    A committed held-out set is a cross-run artefact: the authoring run shipped
    it for a LATER convergence gate, because the ephemeral seal dir dies with
    the run that made it — the tree is the only durable home the set has. No
    git, or no repository: not committed — the authoring-run flow, where the
    set exists only as fresh files.
    """
    code, out = run("git ls-files -- mutants/holdout", gm_dir, timeout=60)
    return code == 0 and bool(out.strip())


def seal_holdout(gm_dir, sealed_dir):
    """Move the held-out set OUT of the worktree, once, at the first gate.

    The seal was a sentence in a skill and nothing enforced it: on the first
    real run the campaign simply executed the held-out mutants itself, learned
    which ones escaped, and could then harden against them — which is exactly
    the overfitting the set exists to prevent. Relocating them makes the seal
    mechanical from the second pass onward, which is where hardening actually
    compounds.

    A COMMITTED set is the exception, and it is left exactly where it is: it
    awaits its own convergence gate. Sealing it here would strip tracked files
    out of the tree — uncommitted deletions a finalize refuses to merge — and
    burn the set's single scoring on a gate that does not own it. The gate
    that DOES own it opts in explicitly with GM_SEAL_COMMITTED=1; the flag can
    only widen what the gate consumes, never soften a verdict.

    Returns True when the set now lives outside the workspace.
    """
    src = os.path.join(gm_dir, "mutants", "holdout")
    if os.path.isdir(src) and holdout_committed_in_tree(gm_dir) \
            and not seal_committed_opted_in(gm_dir):
        return bool(os.path.isdir(sealed_dir) and os.listdir(sealed_dir))
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
    code, out = run("git --no-optional-locks status --porcelain", ws, timeout=120)
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


# apply_mutant's code when apply.sh was NOT run because no record could go
# down first. A string on purpose: a returncode is an int, and a shell killed
# by SIGHUP reports -1 — an apply that DID run and must be reverted.
APPLY_REFUSED = "refused"


_git_dir_cache = {}


def git_dir_of(ws):
    """The tree's git dir (a worktree's own, under the main repo's .git), or
    "" when ws is not a git repository — or when git does not answer:
    bounded and guarded, because this runs before the JSON report the
    campaign parses. Cached per workspace path."""
    key = os.path.realpath(ws)
    if key not in _git_dir_cache:
        try:
            p = subprocess.run(["git", "-C", ws, "rev-parse", "--git-dir"],
                               capture_output=True, text=True, timeout=60)
            d = (p.stdout or "").strip() if p.returncode == 0 else ""
        except (OSError, subprocess.SubprocessError):
            d = ""
        if d and not os.path.isabs(d):
            d = os.path.join(key, d)
        _git_dir_cache[key] = os.path.realpath(d) if d else ""
    return _git_dir_cache[key]


def applied_marker_for(ws):
    """Where the harness notes the mutant it has applied and not yet reverted.

    With the TREE, outside what is judged: in the tree's git dir, which lives
    exactly as long as the mutated files do — a container restarted on the
    same bind-mounted worktree keeps both, a copy-based pod's fresh copy has
    neither — whereas a marker in a temp root outlives or predeceases the
    tree it describes (a retry with a fresh /tmp on the same worktree would
    keep the mutant and lose the note). A workspace that is not a git
    repository falls back to the scratch root (GM_SCRATCH, else the system
    temp dir), keyed on the workspace's real path. In a private directory of
    its own: the marker names a script the next gate will execute.
    """
    real = os.path.realpath(ws)
    key = hashlib.sha256(real.encode("utf-8")).hexdigest()[:12]
    root = git_dir_of(ws) or os.environ.get("GM_SCRATCH", tempfile.gettempdir())
    return os.path.join(root, "gm-applied", "gm-applied-%s.json" % key)


def _private_dir_of_ours(d):
    """(ok, why): the marker directory is OURS and PRIVATE — a real directory,
    not a symlink, owned by this uid, no group/other bits. On a shared temp
    root anyone can pre-create the path; a directory that fails the test is
    refused, never repaired: chmod on a foreign directory is EPERM anyway, and
    repairing it would mean trusting what it already holds."""
    try:
        st = os.lstat(d)
    except OSError as e:
        return False, "%s: %s" % (d, e.strerror or e)
    if stat.S_ISLNK(st.st_mode) or not stat.S_ISDIR(st.st_mode):
        return False, "%s is not a directory of its own (a symlink?)" % d
    if st.st_uid != os.geteuid():
        return False, "%s is owned by uid %d, not %d" % (d, st.st_uid, os.geteuid())
    if st.st_mode & 0o077:
        return False, "%s is not private (mode %o)" % (d, stat.S_IMODE(st.st_mode))
    return True, ""


def read_applied_marker(ws):
    """(meta, why). meta is None when there is no marker. A marker that cannot
    be trusted — a directory that is not ours, a symlink, a file of another
    uid, unreadable content — comes back as ({}, why): the caller refuses,
    executes nothing and deletes nothing. The write side keeps the file
    private; the read side proves it, because the file is an instruction to
    run a script."""
    marker = applied_marker_for(ws)
    if not os.path.lexists(marker):
        return None, ""
    ok, why = _private_dir_of_ours(os.path.dirname(marker))
    if not ok:
        return {}, "the marker directory is not the harness's own: %s" % why
    try:
        fd = os.open(marker, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    except OSError as e:
        return {}, "cannot open the marker %s: %s" % (marker, e.strerror or e)
    try:
        st = os.fstat(fd)
    except OSError as e:
        os.close(fd)
        return {}, "cannot stat the marker %s: %s" % (marker, e.strerror or e)
    if not stat.S_ISREG(st.st_mode) or st.st_uid != os.geteuid():
        os.close(fd)
        return {}, "the marker %s is not a regular file of this uid" % marker
    try:
        with os.fdopen(fd, encoding="utf-8") as f:
            meta = json.load(f)
    except (OSError, ValueError, RecursionError) as e:
        # RecursionError too: that is what json.load raises on a deeply nested
        # document, not ValueError, and it would escape as a traceback.
        return {}, "the marker %s is unreadable: %s" % (marker, e)
    if not isinstance(meta, dict):
        return {}, "the marker %s does not hold an object" % marker
    # The TYPES, not only the shape. `dir` goes straight into os.path.realpath
    # and os.path.join: a number, a list or a string with an embedded NUL raises
    # out of main(), and the harness prints a traceback where the campaign
    # expects its one JSON verdict — read as "the harness is broken" rather than
    # "the gate is red". Measured on the real binary with `{"id": "x", "dir": 5}`:
    # exit 1, empty stdout, TypeError on stderr.
    for k in ("id", "dir"):
        v = meta.get(k)
        if v is not None and (not isinstance(v, str) or "\0" in v):
            return {}, "the marker %s holds a %s that is not a usable string" % (marker, k)
    db = meta.get("dirty_before")
    if db is not None and (not isinstance(db, list) or any(not isinstance(q, str) for q in db)):
        return {}, "the marker %s holds a dirty_before that is not a list of paths" % marker
    return meta, ""


def dirty_paths_before_apply(ws):
    """The paths `git status --porcelain` lists right before a mutant goes
    down, recorded in the marker so a later sweep can tell the mutant's
    residue from work that was already uncommitted (an operator's, in record
    mode). None when git did not answer — unknown, which is not "clean".
    Bounded and guarded like every git call that runs before the report, and
    without the optional index refresh: a status that rewrites the index is a
    write into the tree it is only meant to read (under a full disk or a size
    limit it left a corrupt index behind, and every later checkout failed)."""
    code, out = run("git --no-optional-locks status --porcelain", ws, timeout=120)
    if code != 0:
        return None
    return sorted({l[3:] for l in (out or "").splitlines() if l.strip()})


def write_applied_marker(ws, meta):
    """Record the mutant about to be applied. (True, "") — or (False, why),
    and then apply.sh MUST NOT run.

    Refused when a marker is already there: the mutant it names had its
    revert fail in this run, or another harness is gating the same tree.
    Either way the tree is not HEAD, and overwriting the record is the one
    way to lose a leftover for good: the next mutant's clean revert would
    then erase a record that was never its own. Refused, too, when the
    record cannot be written (a read-only, full or foreign scratch root): a
    mutant the harness cannot keep track of is a mutant it must not apply —
    the silent alternative is a net behaving exactly as before this guard,
    with nothing to say so.
    """
    marker = applied_marker_for(ws)
    d = os.path.dirname(marker)
    try:
        os.makedirs(d, mode=0o700, exist_ok=True)
    except OSError as e:
        return False, "cannot create the marker directory %s: %s" % (d, e.strerror or e)
    ok, why = _private_dir_of_ours(d)
    if not ok:
        return False, "the marker directory is not usable: %s" % why
    if os.path.lexists(marker):
        prior, _why = read_applied_marker(ws)
        prior = prior or {}
        return False, ("a mutant is still recorded as applied: %s (%s) — its revert failed "
                       "in this run, or another harness is gating this tree; the tree is not "
                       "HEAD and nothing is applied on top of it"
                       % (prior.get("id") or "?", prior.get("dir") or marker))
    try:
        fd = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    except OSError as e:
        return False, "cannot create the marker %s: %s" % (marker, e.strerror or e)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump({"id": meta.get("id"), "dir": meta.get("dir"),
                       "dirty_before": dirty_paths_before_apply(ws)}, f)
    except OSError as e:
        # The empty shell a failed write leaves behind records NOTHING — apply.sh
        # will not run — but the NEXT gate reads it as a corrupt marker: kind
        # `unusable`, a bail, and every application refused until a human deletes
        # it. A scratch root full for one instant would wedge the gate for good,
        # over a mutant that was never applied. The open above is O_EXCL, so this
        # file is ours to remove and removing it takes no record with it.
        try:
            os.unlink(marker)
        except OSError:
            pass
        return False, "cannot write the marker %s: %s" % (marker, e.strerror or e)
    return True, ""


def drop_applied_marker(ws, meta):
    """Remove the marker — only when it records THIS mutant, and only after its
    revert demonstrably succeeded. A revert of B must never erase the record of
    A, whose revert failed: that record is the only trace of a tree that is not
    HEAD. There is deliberately no "drop whatever is there" form — dropping a
    record the harness did not just undo is how a leftover is lost for good."""
    prior, why = read_applied_marker(ws)
    if prior is None or why:
        return
    if prior.get("id") is not None and prior.get("id") != meta.get("id"):
        return
    try:
        os.remove(applied_marker_for(ws))
    except OSError:
        pass


def leftover_on_record(ws):
    """True when a mutant is still recorded as applied — the tree is no longer
    KNOWN to be at baseline.

    The single signal, whatever armed it, and that is the point: `revert_clean`
    only exists on a verdict that reached the full apply→capture→revert path.
    Two other ways a revert fails to restore the baseline never set it — the
    inert branch and the failed-apply branch each call revert_mutant and drop
    its exit code on the floor. The record is what all three leave behind, so
    the scoring reads the record and not one verdict's field.
    """
    meta, _why = read_applied_marker(ws)
    return meta is not None


def scoring_must_stop(verdict, ws):
    """The scoring ends after this verdict: the tree is no longer known to be
    at baseline, so every later measurement would describe a program nobody
    wrote — and every later application is refused anyway while the record
    stands. Named, rather than inline in each of the two scoring loops, so the
    rule is one expression and the self-test can drive the decision itself."""
    return (not verdict.get("revert_clean", True)) or leftover_on_record(ws)


def overall_revert_clean(verdicts, stopped, ws):
    """The report's `revert_clean`: did every mutant this gate applied get
    undone, and did the tree come back?

    Three branches never reach the revert path and so never write the field —
    a refused apply, an inert mutant, an apply that failed — and reading their
    SILENCE as True is what let a lot in which almost nothing was measured
    report a clean revert. Combined with a scoring that stopped (so the later
    mutants are absent, `blind_lanes` is empty and the held-out loop never ran,
    leaving a vacuous 0 == 0), that produced a GREEN gate on a tree the harness
    itself had left mutated with its marker armed. A stop, or a record still
    standing, is the evidence the tree did not come back.
    """
    if stopped is not None or leftover_on_record(ws):
        return False
    return all(v.get("revert_clean", True) for v in verdicts)


def apply_mutant(meta, ws):
    # The marker goes down BEFORE apply.sh runs: an interruption between the
    # two leaves a tree that is mutated and a note that says by what. A marker
    # that cannot go down keeps apply.sh from running at all.
    ok, why = write_applied_marker(ws, meta)
    if not ok:
        return APPLY_REFUSED, "apply.sh NOT run: %s" % why
    return run_script(os.path.join(meta["dir"], "apply.sh"), ws)


def revert_mutant(meta, ws):
    code, out = run_script(os.path.join(meta["dir"], "revert.sh"), ws)
    if code == 0:
        drop_applied_marker(ws, meta)
    return code, out


def _under(path, root):
    """True when path (realpath'd) sits under root (realpath'd)."""
    if not root:
        return False
    p, r = os.path.realpath(path), os.path.realpath(root)
    return p == r or p.startswith(r.rstrip(os.sep) + os.sep)


def leftover_roots(ws, gm_dir=None):
    """The only two places a mutant of this workspace's net can live: the
    net's own directory (visible mutants, an unsealed held-out — the whole
    tree when the caller has no net dir to name) and the sealed held-out
    pile (sealed_dir_for — GM_SEALED_DIR honoured). Never the whole scratch
    root: on a shared host that is all of /tmp, and a marker naming a
    directory there would have the gate run whatever script it found."""
    return [gm_dir or ws, sealed_dir_for(ws)]


def revert_leftover_mutant(ws, gm_dir=None):
    """Revert the mutant a previous, interrupted run left applied — if any.

    A stream cut, a SIGTERM or a pod kill between apply.sh and revert.sh
    leaves the mutant in the tree; the next gate, in the same tree, would
    judge a mutated program and call it the lot's. Measured: a tool node
    retried on the same tree after its exec stream broke — the build gate
    went red on a file the mutant had edited, and the oracle reported that
    file as "not committed". Run in record mode too: references recorded
    over a leftover would seal a program nobody wrote, which is worse than
    an edit lost on a file the mutant had already overwritten.

    Returns None when there is nothing to revert. Otherwise a dict with the
    mutant's id and dir, `code` (revert.sh's exit; None when the script is
    gone), `marker`, and after a clean revert `dirty` — what `git status`
    still shows, because the script's exit code is its word, not the tree's
    — or `dirty_unknown`, the reason git could not be asked, which is not
    the same fact as an empty `dirty`.
    The marker is dropped only when the revert demonstrably succeeded — a
    leftover the harness could not revert stays recorded, so the next gate
    reports it again instead of forgetting it; an operator who reverts by
    hand clears it by deleting the marker.

    Refused, nothing executed AND nothing deleted — `refused` names which
    half the harness would not touch: "dir", a marker whose directory is
    neither this workspace nor its sealed held-out pile, and "slot", a
    marker slot that is not the harness's own. Reading is not deciding: the
    record goes down BEFORE apply.sh runs, so it stands whatever the
    directory resolves to today, and dropping it would destroy the only
    evidence that the tree is not HEAD. Every application is refused while
    it is there, which is what makes the refusal stick.

    ONE gate per tree is ASSUMED: the record carries no liveness, no pid and
    no lock. A second gate starting while the first sits between its apply.sh
    and its revert.sh reads that record as a leftover, reverts a LIVE mutant
    and drops its owner's record; the first then captures against a reverted
    tree and reports blind lanes it invented. write_applied_marker's refusal
    ("another harness is gating this tree") does NOT cover this — the sweep
    runs before any application, so it never reaches that refusal. Measured.
    Two harnesses mutating one tree cannot measure anything either way, so
    this is a limitation rather than a mode to support; the fix, if a real
    case turns up, is an exclusive lock held for the gate's duration (an
    flock dies with the process, unlike a recorded pid).
    """
    marker = applied_marker_for(ws)
    meta, why = read_applied_marker(ws)
    if meta is None:
        return None
    if why:
        return {"id": None, "dir": "", "code": None, "out": why, "marker": marker,
                "refused": "slot", "kept": True, "dirty": ""}
    mdir = meta.get("dir") or ""
    if not mdir or not any(_under(mdir, r) for r in leftover_roots(ws, gm_dir)):
        return {"id": meta.get("id"), "dir": mdir, "code": None,
                "out": " and ".join(leftover_roots(ws, gm_dir)),
                "marker": marker, "refused": "dir", "kept": True, "dirty": ""}
    script = os.path.join(mdir, "revert.sh")
    if os.path.isfile(script):
        code, out = run_script(script, ws)
    else:
        code, out = None, "the mutant's revert.sh is no longer there: %s" % script
    dirty, unknown, residue, residue_undecided = "", "", [], False
    if code == 0:
        # Through run(), and on a short leash. This sweep runs in EVERY mode,
        # record included, and the harness's contract is to answer with a
        # verdict: a `git` that is absent must not replace the JSON report with
        # a traceback the campaign reads as "the harness is broken", and a
        # wedged one must not hang the gate the way the incident behind this
        # whole guard hung it. Unanswered is reported as UNKNOWN, never as
        # clean — an empty porcelain and an absent git are not the same fact.
        st, st_out = run("git --no-optional-locks status --porcelain", ws, timeout=120)
        if st == 0:
            dirty = st_out.strip()
            before = meta.get("dirty_before")
            after = sorted({l[3:] for l in st_out.splitlines() if l.strip()})
            if before is not None:
                # What is still modified now and was NOT when the mutant went
                # down is the mutant's residue: revert.sh's exit 0 is the
                # script's word, this is the tree's. An operator's own
                # uncommitted work was there before and is not residue —
                # record mode is where the difference matters, since nothing
                # else stops references from being sealed over a leftover.
                residue = sorted(set(after) - set(before))
            elif after:
                # No record of what was dirty before the mutant went down
                # (git did not answer then, or the marker predates the
                # field): a tree that still shows changes is UNDECIDABLE,
                # and undecidable is not clean — every change is treated as
                # residue, the marker stays, an operator decides.
                residue = after
                residue_undecided = True
        else:
            unknown = "`git status` did not answer (exit %s)" % st
        if not residue:
            drop_applied_marker(ws, meta)
    return {"id": meta.get("id"), "dir": mdir, "code": code, "out": (out or "")[-300:],
            "marker": marker, "refused": False, "kept": code != 0 or bool(residue),
            "dirty": dirty[:400], "dirty_unknown": unknown, "residue": residue[:50],
            "residue_undecided": residue_undecided}


# The dispositions on which the gate STOPS rather than judges. Only "reverted"
# — the harness undid the leftover and saw the tree come back — lets it
# through. The set lives here, beside the function that produces the kinds, so
# the decision and its consumer cannot drift apart silently.
LEFTOVER_BAIL_KINDS = ("still_mutated", "unusable", "refused")


def leftover_disposition(left):
    """What the gate says about a leftover, and whether it may proceed.

    Returns (kind, text): "reverted" — revert.sh exit 0, a note (with what
    the tree still shows, if anything), the only kind that lets the gate
    through; "refused" — a marker of ours naming a directory the harness
    does not recognise, kept and not executed; "unusable" — the marker slot
    is not the harness's own; "still_mutated" — the revert failed or its
    script is gone. The last three are LEFTOVER_BAIL_KINDS: the tree is, or
    may be, not HEAD, and the gate refuses rather than judge a program
    nobody wrote.
    """
    if left.get("refused") == "slot":
        return "unusable", (
            "the application-marker slot %s is not the harness's own: %s. Nothing "
            "executed, nothing deleted. Every mutant application is refused until "
            "it is removed by hand or GM_SCRATCH points at a private root."
            % (left.get("marker"), left.get("out")))
    if left.get("refused"):
        return "refused", (
            "a mutant is recorded as APPLIED (%s) and the harness will NOT revert it: "
            "its directory %s is none of the roots a leftover may live under (%s). "
            "Nothing executed, nothing deleted. The record goes down BEFORE apply.sh "
            "runs, so it stands whatever that directory resolves to today: the tree "
            "is, or may be, mutated, and the gate will not judge it. The ordinary "
            "cause is a GM_SEALED_DIR that moved between passes — re-run with the one "
            "that pass used and the harness reverts the mutant itself. Otherwise "
            "revert by hand (git checkout -- <the mutant's paths>), then delete the "
            "marker %s."
            % (left.get("id") or "?", left.get("dir") or "?", left.get("out") or "?",
               left.get("marker")))
    if left.get("code") == 0 and left.get("residue"):
        if left.get("residue_undecided"):
            return "still_mutated", (
                "a mutant left APPLIED by an interrupted gate was reverted at start (%s, revert.sh "
                "exit 0), but the tree still shows changes and nothing recorded what was already "
                "uncommitted when the mutant went down, so their origin is UNDECIDABLE: %s. "
                "Undecidable is not clean — the tree will be neither judged nor recorded. Check "
                "those paths (git diff), revert the mutant's by hand, then delete the marker %s."
                % (left.get("id"), ", ".join(left["residue"][:20]), left.get("marker")))
        return "still_mutated", (
            "a mutant left APPLIED by an interrupted gate was NOT fully reverted at start: %s "
            "— revert.sh exited 0, but these paths are still modified and were clean when the "
            "mutant went down: %s. The tree is still mutated and will be neither judged nor "
            "recorded. Revert by hand (git checkout -- <those paths>), then delete the marker %s."
            % (left.get("id"), ", ".join(left["residue"][:20]), left.get("marker")))
    if left.get("code") == 0:
        text = ("a mutant left APPLIED by an interrupted gate was reverted at start: %s "
                "(revert.sh exit 0). The interrupted attempt's verdict, if any, judged a "
                "mutated program." % left.get("id"))
        if left.get("dirty"):
            text += (" The tree still shows uncommitted changes after the revert — the "
                     "script's exit is its word, not the tree's: %s"
                     % " ".join(left["dirty"].split())[:300])
        elif left.get("dirty_unknown"):
            text += (" Whether the tree actually came back could not be checked: %s. "
                     "Unknown, not clean." % left["dirty_unknown"])
        return "reverted", text
    how = "revert.sh is missing" if left.get("code") is None else "revert.sh exited %s" % left.get("code")
    return "still_mutated", (
        "a mutant left APPLIED by an interrupted gate could NOT be reverted at start: %s (%s). "
        "The tree is still mutated and will not be judged. Revert by hand (git checkout -- "
        "<the mutant's paths>), then delete the marker %s." % (left.get("id"), how, left.get("marker")))


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

def probe_mutation(meta, ws):
    """Apply the mutant and decide whether it CHANGED anything.

    Returns (verdict, state) with state in {"mutated", "inert", "failed"}, and
    leaves the mutant APPLIED when it mutated: the caller owns the revert,
    because the gate has work to do in that state and this probe has none.

    Extracted so that the gate and the cheap validity check decide by the SAME
    code. Mechanical validity is what stops a no-op mutant from either inflating
    or diluting the score, so two implementations of it would eventually
    disagree about what counts as a mutation — and the one used to accept a
    repair would be the lenient one.
    """
    targets = list(meta.get("targets") or [])
    base = {"id": meta["id"], "class": meta.get("class", "unknown"),
            "archetype": meta.get("archetype", "unknown"),
            "surface": meta.get("surface", "http"),
            "targets_declared": targets}

    if not targets:
        return dict(base, valid=False, detected=False,
                    reason="meta.json declares no targets"), "failed"

    before_tree, before_data = tree_fingerprint(ws), data_fingerprint(meta, ws)

    code, out = apply_mutant(meta, ws)
    if code == APPLY_REFUSED:
        # Nothing ran: no half-edit to undo, and the marker down there is
        # another mutant's record of a tree that is not HEAD — this mutant's
        # revert.sh over it would erase that record and half-restore the tree.
        return dict(base, valid=False, detected=False, reason=out[-300:]), "failed"
    if code != 0:
        # A failed apply may have half-edited the tree, and the marker is
        # down: revert now, within this run, so neither outlives it — a
        # marker left here would have the NEXT gate run this revert.sh over
        # whatever the operator wrote since.
        revert_mutant(meta, ws)
        return dict(base, valid=False, detected=False,
                    reason="apply.sh exited %s: %s" % (code, out[-300:])), "failed"

    after_tree, after_data = tree_fingerprint(ws), data_fingerprint(meta, ws)
    mutated = (before_tree != after_tree) or (
        before_data is not None and before_data != after_data
    )

    if not mutated:
        # A mutant that changes nothing is a measurement fault, not evidence.
        return dict(base, valid=False, detected=False,
                    reason="apply.sh left the tree and the data probe unchanged"), "inert"

    return dict(base, valid=True), "mutated"


def score_mutant(meta, config, corpus, canon, refs, ws, seed):
    """Apply → capture → compare → revert. Returns a per-mutant verdict dict."""
    verdict, state = probe_mutation(meta, ws)
    if state == "failed":
        return verdict
    if state == "inert":
        revert_mutant(meta, ws)
        return verdict

    targets = verdict["targets_declared"]

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
    # what makes the finding actionable instead of a bare count. Whether it IS
    # the mutant is settled after the revert, below — not assumed here.
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

    # THE THIRD CAUSE, and it has to be TESTED rather than reasoned about.
    #
    # A control entry that differs from its reference was attributed to the
    # mutant, and the message offered two explanations: an under-declared blast
    # radius, or a capture that is not isolated. There is a third, and it is
    # the only one that does not involve the mutant at all — THE CONTROL ENTRY
    # DOES NOT REPRODUCE ITSELF. Whichever mutant happened to sample it wears
    # the blame, which sends the reader looking for a causal path that does not
    # exist. Measured: a mutant that rewrites a department projection was
    # reported as moving a users list, whose controller only ever builds
    # regions; no code path connected the two.
    #
    # The revert already re-captures. Re-capturing the DRIFTED CONTROLS at the
    # same time costs nothing and settles it: with the mutant gone, an entry
    # that still differs was never moved by it.
    #
    # `stable` cannot cover this. It compares three captures to EACH OTHER, all
    # taken within one run, so an entry that drifts occasionally passes it —
    # and the whole-corpus negative control catches permanent drift, not rare
    # drift.
    suspect = list(verdict.get("collateral") or [])
    back = capture(config, corpus, canon, ids=set(targets) | set(suspect))
    verdict["revert_clean"] = not diverged(refs, back, targets)
    if not verdict["revert_clean"]:
        verdict["reason"] = "the tree did not return to the reference after revert.sh"

    unstable = diverged(refs, back, suspect)
    if unstable:
        verdict["unstable_controls"] = unstable
        verdict["collateral"] = [c for c in suspect if c not in unstable]
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


# ─── Self-test ──────────────────────────────────────────────────────────────

def _selftest():
    """GM_MODE=selftest — teste la moitie qui DECIDE, pas celle qui compare.

    Le canonicaliseur a ses propres tests, ecrits par la campagne : ils portent
    sur ce qui est EFFACE avant de comparer. Ceux-ci portent sur ce qui en est
    CONCLU, et cette moitie n'appartient pas a la campagne. Une regle de
    decision fausse rend vert un filet dont chaque comparaison est juste.

    Ils vivent DANS ce fichier parce que ce fichier se recopie lui-meme dans le
    depot cible : des tests a cote ne suivraient pas, et le filet materialise
    embarquerait alors une regle de decision que plus rien ne verifie.

    Les collaborateurs sont doubles ; c'est `score_mutant` et `probe_mutation`
    qui tournent, jamais une reimplementation — qui finirait par diverger d'eux.
    """
    failures, checked = [], [0]

    def check(name, got, want):
        checked[0] += 1
        if got != want:
            failures.append("%s\n    attendu : %r\n    obtenu  : %r" % (name, want, got))

    g = globals()
    saved = {k: g[k] for k in ("apply_mutant", "revert_mutant", "tree_fingerprint",
                               "data_fingerprint", "app_restart", "capture", "control_ids")}

    class World:
        """Une application doublee.

        `unstable` designe les entrees qui different de leur reference MEME
        quand aucun mutant n'est applique — celles qui ne se reproduisent pas.
        """

        def __init__(self, refs, moved_when_applied, unstable=()):
            self.refs, self.applied = dict(refs), False
            self.moved, self.unstable = set(moved_when_applied), set(unstable)

        def capture(self, config, corpus, canon, ids=None):
            ids = set(ids or self.refs)
            return {i: self.refs[i] + ("!" if (
                (i in self.moved) if self.applied else (i in self.unstable)) else "")
                for i in ids}

    def score(targets, sample, moved, unstable=(), revert_code=0):
        ids = ["%03d" % n for n in range(1, 13)]
        refs = {i: "ref-" + i for i in ids}
        corpus = {"entries": [{"id": i, "surface": "http"} for i in ids]}
        meta = {"id": "t", "dir": "/dev/null", "class": "code", "surface": "http",
                "archetype": "value_change", "targets": list(targets), "needs_restart": False}
        w = World(refs, moved, unstable)
        n = [0]

        def tree(_ws):
            # Deux empreintes distinctes de part et d'autre de l'application :
            # c'est tout ce que la validite mecanique demande.
            n[0] += 1
            return "applied" if w.applied else "clean-%d" % n[0]

        def apply_(_m, _w):
            w.applied = True
            return 0, ""

        def revert_(_m, _w):
            w.applied = False
            return revert_code, ("" if revert_code == 0 else "boom")

        g.update(apply_mutant=apply_, revert_mutant=revert_, tree_fingerprint=tree,
                 data_fingerprint=lambda _m, _w: None, app_restart=lambda _c, _w: None,
                 capture=w.capture, control_ids=lambda _c, _t, _s: list(sample))
        return score_mutant(meta, {}, corpus, None, refs, "/dev/null", 0)

    try:
        # 1. Un mutant qui ne bouge QUE ses cibles.
        v = score(["001"], ["005", "006"], ["001"])
        check("cible bougee -> detecte", v["detected"], True)
        check("aucun collateral", v["collateral"], [])
        check("aucun temoin instable", v.get("unstable_controls"), None)
        check("revert propre", v["revert_clean"], True)

        # 2. Rayon d'action SOUS-DECLARE : le temoin revient quand le mutant
        #    part, donc c'est bien le mutant qui l'avait bouge.
        v = score(["001"], ["005", "006"], ["001", "005"])
        check("collateral reel nomme", v["collateral"], ["005"])
        check("collateral reel non dit instable", v.get("unstable_controls"), None)
        check("revert propre malgre le collateral", v["revert_clean"], True)

        # 3. LA DISCRIMINATION : un temoin qui differe AUSSI sans le mutant.
        v = score(["001"], ["005", "006"], ["001", "005"], unstable=["005"])
        check("temoin instable nomme", v.get("unstable_controls"), ["005"])
        check("et retire du collateral", v["collateral"], [])
        check("le mutant reste detecte", v["detected"], True)
        check("revert toujours propre", v["revert_clean"], True)

        # 4. Les deux a la fois, sans se confondre.
        v = score(["001"], ["005", "006"], ["001", "005", "006"], unstable=["006"])
        check("collateral reel garde", v["collateral"], ["005"])
        check("instable separe", v.get("unstable_controls"), ["006"])

        # 5. Un revert casse n'est pas masque par la discrimination.
        v = score(["001"], ["005"], ["001", "005"], revert_code=3)
        check("revert casse -> revert_clean faux", v["revert_clean"], False)
        check("et la cause est dite", "revert.sh exited 3" in (v.get("reason") or ""), True)

        # 6. Validite mecanique : un apply qui ne change rien est INVALIDE,
        #    jamais « non detecte » — il ne peut ni gonfler ni diluer le score.
        g.update(apply_mutant=lambda _m, _w: (0, ""), tree_fingerprint=lambda _w: "identique",
                 data_fingerprint=lambda _m, _w: None, revert_mutant=lambda _m, _w: (0, ""))
        v, state = probe_mutation({"id": "t", "dir": "/dev/null", "targets": ["001"]}, "/dev/null")
        check("apply inerte -> etat inerte", state, "inert")
        check("apply inerte -> invalide", v["valid"], False)
        v, state = probe_mutation({"id": "t", "dir": "/dev/null", "targets": []}, "/dev/null")
        check("sans cible -> echec", state, "failed")
        check("sans cible -> cause dite", "no targets" in v.get("reason", ""), True)

        # 7. Sondes de corpus : un tag sans la substance mecanique ne compte pas.
        probes_cfg = {"personas": [
            {"name": "admin", "login": {"fields": {"email": "a@x", "password": "p"}}},
            {"name": "admin-case", "case_variant_of": "admin",
             "login": {"fields": {"email": "A@X", "password": "p"}}}]}
        full = {"entries": [
            {"id": "001", "surface": "write", "method": "POST", "path": "/w",
             "probes": ["write_create"], "readback": "/r"},
            {"id": "002", "surface": "write", "method": "POST", "path": "/w2",
             "probes": ["error_then_corrected"], "readback": "/r",
             "steps": [{"fields": {"a": ""}}, {"fields": {"a": "b"}}]},
            {"id": "003", "path": "/list?q=Teacher", "probes": ["case_pair"]},
            {"id": "004", "path": "/list?q=teacher", "probes": ["case_pair"]},
            {"id": "005", "path": "/list?sort=name,asc", "probes": ["text_sort"]},
            {"id": "006", "path": "/board", "persona": "admin-case"},
        ]}
        check("toutes sondes presentes -> aucun manque",
              missing_corpus_probes(full, probes_cfg), [])
        deg = json.loads(json.dumps(full))
        deg["entries"].pop()   # la variante existe mais aucune entree ne l'observe
        check("variante de casse jamais observee -> manque",
              [x["probe"] for x in missing_corpus_probes(deg, probes_cfg)],
              ["auth_case"])
        deg = json.loads(json.dumps(full))
        deg["entries"][0]["method"] = "GET"
        check("creation en GET -> ne compte pas",
              [x["probe"] for x in missing_corpus_probes(deg, probes_cfg)],
              ["write_create"])
        deg = json.loads(json.dumps(full))
        deg["entries"][1].pop("steps")
        check("sequence sans steps -> manque nomme",
              [x["probe"] for x in missing_corpus_probes(deg, probes_cfg)],
              ["error_then_corrected"])
        deg = json.loads(json.dumps(full))
        deg["entries"][3]["path"] = "/list?q=Teacher"
        check("paire de casse sans difference -> manque",
              [x["probe"] for x in missing_corpus_probes(deg, probes_cfg)],
              ["case_pair"])
        deg_cfg = json.loads(json.dumps(probes_cfg))
        deg_cfg["personas"][1]["login"]["fields"]["email"] = "a@x"
        check("login variante sans difference de casse -> manque",
              [x["probe"] for x in missing_corpus_probes(full, deg_cfg)],
              ["auth_case"])

        # 8. Perimetre : motifs, methode, slash final jamais plie, exclusions.
        routes = [{"method": "GET", "pattern": "/list"},
                  {"method": None, "pattern": "/items/{id}"},
                  {"method": "GET", "pattern": "/admin/"},
                  {"method": "POST", "pattern": "/w"}]
        rcorpus = {"entries": [{"id": "1", "path": "/list?page=2"},
                               {"id": "2", "path": "/items/42"},
                               {"id": "3", "method": "POST", "path": "/w"},
                               {"id": "4", "path": "/admin"}]}
        check("motif {id} et query couverts, slash final NON plie",
              route_gaps(routes, rcorpus, {}), ["GET /admin/"])
        check("exclusion justifiee honoree",
              route_gaps(routes, rcorpus, {"/admin/": "retiree du perimetre"}), [])
        check("exclusion transcrite du message de refus (METHOD pattern) honoree",
              route_gaps(routes, rcorpus, {"GET /admin/": "retiree du perimetre"}), [])
        check("l'exclusion d'une autre methode n'excuse pas la route",
              route_gaps(routes, rcorpus, {"POST /admin/": "retiree du perimetre"}),
              ["GET /admin/"])
        check("un GET ne couvre pas un POST",
              route_gaps([{"method": "POST", "pattern": "/list"}], rcorpus, {}),
              ["POST /list"])

        # 8b. Features : l'etage au-dessus des routes — non-mappee, perimee,
        #     preuve inexistante, exigence de deux sources.
        fcorpus = {"entries": [{"id": "1"}, {"id": "2"}]}
        feats = {"catalog.search": ["i18n", "nav"], "catalog.export": ["nav"]}
        fcov = {"features": [{"feature": "catalog.search", "entries": ["1", "2"]}],
                "exclusions": [{"feature": "catalog.export", "reason": "retiree"}]}
        check("inventaire complet -> aucun trou",
              feature_gaps(feats, fcov, fcorpus), ([], [], []))
        check("feature ni mappee ni exclue -> nommee",
              feature_gaps(feats, {"features": fcov["features"]}, fcorpus)[0],
              ["catalog.export"])
        check("inventaire d'une feature disparue -> perime",
              feature_gaps({"catalog.search": ["nav", "i18n"]}, fcov, fcorpus)[1],
              ["catalog.export"])
        check("mapping vers une entree inexistante -> preuve absente",
              feature_gaps(feats, {"features": [{"feature": "catalog.search",
                                                 "entries": ["9"]}]}, fcorpus)[2],
              ["catalog.search -> 9"])
        try:
            parse_feature_probe("a nav\nb nav\n")
            check("sonde mono-source -> refus", "aucun-refus", "SystemExit")
        except SystemExit:
            check("sonde mono-source -> refus", "SystemExit", "SystemExit")
        try:
            parse_feature_probe("# rien\n")
            check("sonde vide -> refus", "aucun-refus", "SystemExit")
        except SystemExit:
            check("sonde vide -> refus", "SystemExit", "SystemExit")
        check("sources fusionnees par feature, commentaires ignores",
              parse_feature_probe("a nav\n# c\na i18n\nb i18n\n"),
              {"a": ["i18n", "nav"], "b": ["i18n"]})

        # 8e. Garde-fous de forme : inventaire malforme = refus nomme, jamais
        #     une traceback ; opt-in mal type = refus nomme, jamais un silence ;
        #     le standard est un cliquet — les deux sens de derive refusent.
        def named_refusal(name, fn):
            try:
                fn()
                check(name, "aucun-refus", "SystemExit")
            except SystemExit:
                check(name, "SystemExit", "SystemExit")
        named_refusal("features: liste de chaines -> refus nomme",
                      lambda: validate_feature_coverage({"features": ["a"]}))
        named_refusal("entries: chaine -> refus nomme",
                      lambda: validate_feature_coverage(
                          {"features": [{"feature": "a", "entries": "12"}]}))
        check("inventaire null normalise en listes",
              validate_feature_coverage({"features": None, "exclusions": None}),
              {"features": [], "exclusions": []})
        sdir2 = tempfile.mkdtemp(prefix="gm-selftest-optin-")
        with open(os.path.join(sdir2, "config.json"), "w", encoding="utf-8") as f:
            f.write('{"seal_committed": "true"}')
        named_refusal("seal_committed chaine -> refus nomme, pas un silence",
                      lambda: seal_committed_opted_in(sdir2))
        named_refusal("reason non-string -> refus nomme",
                      lambda: validate_feature_coverage(
                          {"exclusions": [{"feature": "a", "reason": 42}]}))
        named_refusal("feature non-string -> refus nomme",
                      lambda: validate_feature_coverage(
                          {"features": [{"feature": 42, "entries": ["1"]}]}))
        named_refusal("feature mappee deux fois -> refus nomme",
                      lambda: validate_feature_coverage(
                          {"features": [{"feature": "a", "entries": ["1"]},
                                        {"feature": "a", "entries": ["2"]}]}))
        named_refusal("feature exclue deux fois -> refus nomme",
                      lambda: validate_feature_coverage(
                          {"exclusions": [{"feature": "a", "reason": "x"},
                                          {"feature": "a", "reason": "y"}]}))
        named_refusal("feature mappee ET exclue -> refus nomme",
                      lambda: validate_feature_coverage(
                          {"features": [{"feature": "a", "entries": ["1"]}],
                           "exclusions": [{"feature": "a", "reason": "x"}]}))
        prev_env = os.environ.pop("GM_SEAL_COMMITTED", None)
        os.environ["GM_SEAL_COMMITTED"] = "yes"
        try:
            named_refusal("GM_SEAL_COMMITTED=yes -> refus nomme, pas un silence",
                          lambda: seal_committed_opted_in(tempfile.mkdtemp()))
        finally:
            if prev_env is None:
                os.environ.pop("GM_SEAL_COMMITTED", None)
            else:
                os.environ["GM_SEAL_COMMITTED"] = prev_env
        named_refusal("`**` hors queue -> refus nomme",
                      lambda: route_regex("/x/**/y"))
        check("`**` en queue -> matche la queue",
              bool(route_regex("/x/**").match("/x/a/b")), True)
        udir = tempfile.mkdtemp(prefix="gm-selftest-umark-")
        upath = os.path.join(udir, "standard-mark")
        with open(upath, "w", encoding="utf-8") as f:
            f.write("3")
        os.chmod(upath, 0)
        check("standard-mark illisible -> bail, jamais une traceback",
              standard_mark_verdict(udir, 3)[0], "bail")
        os.chmod(upath, 0o600)
        mdir = tempfile.mkdtemp(prefix="gm-selftest-mark-")
        with open(os.path.join(mdir, "standard-mark"), "w", encoding="utf-8") as f:
            f.write("3\n")
        check("standard sous la marque -> bail (retrogradation nommee)",
              standard_mark_verdict(mdir, 2)[0], "bail")
        check("standard au-dessus de la marque -> bail (paire desynchronisee)",
              standard_mark_verdict(mdir, 4)[0], "bail")
        check("standard egal a la marque -> ok",
              standard_mark_verdict(mdir, 3), ("ok", ""))
        check("standard 3 sans marque -> note",
              standard_mark_verdict(tempfile.mkdtemp(prefix="gm-nm-"), 3)[0],
              "note")

        # 8c. Scellement : un jeu frais se scelle ; un jeu COMMITTE attend sa
        #     propre porte (le sceller brulerait sa notation et laisserait des
        #     suppressions non committees) ; l'opt-in est explicite.
        sroot = tempfile.mkdtemp(prefix="gm-selftest-seal-")
        def mk_holdout(repo):
            hd = os.path.join(repo, ".golden-master", "mutants", "holdout", "t01")
            os.makedirs(hd)
            with open(os.path.join(hd, "apply.sh"), "w", encoding="utf-8") as f:
                f.write("true\n")
            return os.path.join(repo, ".golden-master")
        gmd = mk_holdout(os.path.join(sroot, "fresh"))
        sealed1 = os.path.join(sroot, "sealed1")
        check("jeu non committe -> scelle hors de l'arbre",
              [seal_holdout(gmd, sealed1),
               os.path.isdir(os.path.join(gmd, "mutants", "holdout", "t01"))],
              [True, False])
        repo2 = os.path.join(sroot, "committed")
        gmd2 = mk_holdout(repo2)
        for cmd in ("git init -q", "git add -A",
                    "git -c user.email=t@t -c user.name=t commit -qm seed"):
            run(cmd, repo2, timeout=60)
        sealed2 = os.path.join(sroot, "sealed2")
        check("jeu committe -> laisse en place, rien de scelle",
              [seal_holdout(gmd2, sealed2),
               os.path.isdir(os.path.join(gmd2, "mutants", "holdout", "t01")),
               os.path.isdir(sealed2)],
              [False, True, False])
        prev_seal = os.environ.get("GM_SEAL_COMMITTED")
        os.environ["GM_SEAL_COMMITTED"] = "1"
        try:
            check("opt-in explicite -> le jeu committe se scelle",
                  [seal_holdout(gmd2, sealed2),
                   os.path.isdir(os.path.join(gmd2, "mutants", "holdout", "t01"))],
                  [True, False])
        finally:
            if prev_seal is None:
                os.environ.pop("GM_SEAL_COMMITTED", None)
            else:
                os.environ["GM_SEAL_COMMITTED"] = prev_seal
        repo3 = os.path.join(sroot, "config-form")
        gmd3 = mk_holdout(repo3)
        with open(os.path.join(gmd3, "config.json"), "w", encoding="utf-8") as f:
            f.write('{"seal_committed": true}')
        for cmd in ("git init -q", "git add -A",
                    "git -c user.email=t@t -c user.name=t commit -qm seed"):
            run(cmd, repo3, timeout=60)
        check("opt-in par config.json -> le jeu committe se scelle",
              [seal_holdout(gmd3, os.path.join(sroot, "sealed3")),
               os.path.isdir(os.path.join(gmd3, "mutants", "holdout", "t01"))],
              [True, False])

        # 8d. Registre : une demande sans acte bloque ; un `replaces` acte la
        #     chaine ; un bloc illisible escalade, ne se devine pas.
        ldir = tempfile.mkdtemp(prefix="gm-selftest-ledger-")
        lp = os.path.join(ldir, "REBASELINE.md")

        def ledger(*blks):
            with open(lp, "w", encoding="utf-8") as f:
                f.write("\n".join("<!-- iterion:rebaseline-%s\n%s\n-->" % b for b in blks))

        check("pas de registre -> aucun pending",
              pending_rebaselines(os.path.join(ldir, "absent")), [])
        ledger(("request", '{"id": "R-A-1", "lot": "A"}'))
        check("demande sans acte -> pending",
              [p["id"] for p in pending_rebaselines(ldir)], ["R-A-1"])
        ledger(("request", '{"id": "R-A-1", "lot": "A"}'),
               ("act", '{"id": "R-A-1", "lot": "A", "recorded_paths": []}'))
        check("demande actee -> rien", pending_rebaselines(ldir), [])
        ledger(("request", '{"id": "R-A-1", "lot": "A"}'),
               ("request", '{"id": "R-B-1", "lot": "B", "replaces": "R-A-1"}'),
               ("act", '{"id": "R-B-1", "lot": "B", "recorded_paths": []}'))
        check("remplacee par une demande actee -> chaine fermee",
              pending_rebaselines(ldir), [])
        ledger(("request", '{"id": "R-A-1", "lot": "A"}'),
               ("request", '{"id": "R-B-1", "lot": "B", "replaces": "R-A-1"}'))
        check("remplacee par une demande NON actee -> les deux pendent",
              sorted(p["id"] for p in pending_rebaselines(ldir)),
              ["R-A-1", "R-B-1"])
        ledger(("request", 'pas du json'))
        check("bloc illisible -> escalade nommee",
              pending_rebaselines(ldir)[0]["id"], "UNPARSEABLE")
        ledger(("request", '[]'))
        check("bloc JSON valide mais non-objet -> escalade nommee, pas une traceback",
              pending_rebaselines(ldir)[0]["id"], "UNPARSEABLE")
        ledger(("request", '{"lot": "1"}'))
        check("bloc objet SANS id -> escalade nommee, pas un KeyError",
              pending_rebaselines(ldir)[0]["id"], "UNPARSEABLE")

        # 8e. Extensions : le verdict additions-only, falsifie dans les deux
        #     sens — une extension legitime DOIT passer, et chaque deguisement
        #     du vecteur de masquage (reecriture, suppression, retouche,
        #     collision, entree passee en fraude, registre reecrit) DOIT
        #     rougir. Un fixture git reel, parce que le verdict se lit en git.
        xroot = tempfile.mkdtemp(prefix="gm-selftest-ext-")
        xgm = os.path.join(xroot, ".golden-master")
        os.makedirs(os.path.join(xgm, "refs"))
        base_entry = {"id": "1", "method": "GET", "path": "/a", "persona": "p"}
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry]}, f)
        with open(os.path.join(xgm, "refs", "1.txt"), "w", encoding="utf-8") as f:
            f.write("ref-1\n")
        for cmd in ("git init -q", "git add -A",
                    "git -c user.email=t@t -c user.name=t commit -qm seed"):
            run(cmd, xroot, timeout=60)
        xbase = run("git rev-parse HEAD", xroot, timeout=60)[1].strip()

        def xledger(*blks):
            with open(os.path.join(xgm, "EXTENSIONS.md"), "w", encoding="utf-8") as f:
                f.write("\n".join("<!-- iterion:extension-%s\n%s\n-->" % b
                                  for b in blks))

        def xcommit():
            run("git add -A", xroot, timeout=60)
            run("git -c user.email=t@t -c user.name=t commit -qm x",
                xroot, timeout=60)

        def xrequest(req):
            """The lot files its request and commits. A fixture that stages a
            request and its act in ONE commit models a sequence production
            cannot produce (the parent commits its lot before the subbot
            starts) — and hides the provenance the verdict checks."""
            xledger(("request", req))
            xcommit()

        def xreset():
            run("git reset -q --hard %s" % xbase, xroot, timeout=60)
            run("git clean -qfd", xroot, timeout=60)

        req2 = ('{"id": "E-1", "lot": "L", "type": "add-file",'
                ' "paths": [".golden-master/refs/2.txt"]}')
        act2 = ('{"id": "E-1", "lot": "L",'
                ' "recorded_paths": [".golden-master/refs/2.txt"]}')

        check("pas de registre -> aucune extension pendante",
              pending_extensions(xgm), [])
        xledger(("request", req2))
        check("demande sans acte -> pendante",
              [p["id"] for p in pending_extensions(xgm)], ["E-1"])
        xledger(("request", req2), ("act", act2))
        check("demande actee -> rien", pending_extensions(xgm), [])
        xledger(("request", 'pas du json'))
        check("bloc extension illisible -> escalade nommee",
              pending_extensions(xgm)[0]["id"], "UNPARSEABLE")

        # Extension legitime : un fichier de ref NEUF, acte, registre commite.
        xreset()
        xrequest(req2)
        xledger(("request", req2), ("act", act2))
        with open(os.path.join(xgm, "refs", "2.txt"), "w", encoding="utf-8") as f:
            f.write("ref-2\n")
        xcommit()
        v = extension_verdict(xroot, ".golden-master", xbase)
        check("add-file legitime -> ok, chemin exempte",
              [v["acted"][0]["ok"], v["ok_paths"]],
              [True, [".golden-master/refs/2.txt"]])

        # Un acte deja present A LA BASE n'est pas re-juge : ses ajouts sont
        # les references de cette base, et les relire comme des reecritures
        # refuserait tout lot parti d'une base qui contient un acte certifie.
        xbase_acted = run("git rev-parse HEAD", xroot, timeout=60)[1].strip()
        with open(os.path.join(xroot, "later.txt"), "w", encoding="utf-8") as f:
            f.write("a lot landed after the act\n")
        xcommit()
        v_after = extension_verdict(xroot, ".golden-master", xbase_acted)
        check("acte present a la base -> ok, acted_at_base, aucun probleme",
              [v_after["acted"][0]["ok"], v_after["acted"][0].get("acted_at_base"),
               v_after["acted"][0]["problems"], v_after["problems"]],
              [True, True, [], []])
        # ... et la reecriture d'une de ses refs par le lot reste vue par le
        # verdict d'immutabilite (git diff), pas par le certificat.
        with open(os.path.join(xgm, "refs", "2.txt"), "w", encoding="utf-8") as f:
            f.write("ref-2-reecrite-par-un-lot\n")
        xcommit()
        v_touch = extension_verdict(xroot, ".golden-master", xbase_acted)
        check("acte a la base + ref reecrite -> le certificat n'exempte rien",
              [v_touch["acted"][0]["ok"], v_touch["ok_paths"]], [True, []])

        # Reecriture deguisee : la ref existait a la base.
        xreset()
        xledger(("request", '{"id": "E-1", "lot": "L", "paths": [".golden-master/refs/1.txt"]}'),
                ("act", '{"id": "E-1", "lot": "L", "recorded_paths": [".golden-master/refs/1.txt"]}'))
        with open(os.path.join(xgm, "refs", "1.txt"), "w", encoding="utf-8") as f:
            f.write("ref-1-reecrite\n")
        xcommit()
        check("reecriture deguisee en ajout -> refusee",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Suppression enregistree : le chemin manque a HEAD (le cote delete
        # d'un renommage perd exactement ici).
        xreset()
        xledger(("request", req2), ("act", act2))
        xcommit()
        check("chemin enregistre absent a HEAD (delete/rename) -> refuse",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Entree de corpus legitime, revendiquee par la demande.
        xreset()
        new_entry = {"id": "2", "method": "GET", "path": "/b", "persona": "p"}
        xrequest('{"id": "E-2", "lot": "L", "type": "add-entry",'
                 ' "corpus_entries": [{"id": "2"}]}')
        xledger(("request", '{"id": "E-2", "lot": "L", "type": "add-entry",'
                            ' "corpus_entries": [{"id": "2"}]}'),
                ("act", '{"id": "E-2", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry, new_entry]}, f)
        xcommit()
        check("add-entry legitime et revendiquee -> ok",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], True)

        # La meme demande dans l'orthographe du registre de re-baseline
        # (`expected_paths` + `entries` en identifiants) : lue au meme point,
        # jugee pareil. Mesure : toute demande conforme a un en-tete de
        # registre ecrit dans cet idiome etait jugee « smuggled ».
        xreset()
        req_taught = ('{"id": "E-2", "lot": "L", "entries": ["2"],'
                      ' "expected_paths": [".golden-master/refs/2.txt"]}')
        xrequest(req_taught)
        xledger(("request", req_taught),
                ("act", '{"id": "E-2", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json",'
                        ' ".golden-master/refs/2.txt"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry, new_entry]}, f)
        with open(os.path.join(xgm, "refs", "2.txt"), "w", encoding="utf-8") as f:
            f.write("ref-2\n")
        xcommit()
        check("demande ecrite comme le registre de re-baseline l'enseigne -> ok",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], True)

        # Retouche d'une entree existante sous couvert d'ajout.
        xreset()
        xledger(("request", '{"id": "E-2", "lot": "L", "corpus_entries": [{"id": "2"}]}'),
                ("act", '{"id": "E-2", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        touched = dict(base_entry, path="/a-bougee")
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [touched, new_entry]}, f)
        xcommit()
        check("entree existante retouchee -> refusee",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Collision : la « nouvelle » entree observe le meme tuple qu'une
        # existante — deux references pour une observation.
        xreset()
        collider = dict(base_entry, id="9")
        xledger(("request", '{"id": "E-3", "lot": "L", "corpus_entries": [{"id": "9"}]}'),
                ("act", '{"id": "E-3", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry, collider]}, f)
        xcommit()
        check("collision de tuple d'observation -> refusee",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Entree passee en fraude : ajoutee au corpus, revendiquee par
        # personne.
        xreset()
        smuggled = {"id": "8", "method": "GET", "path": "/c", "persona": "p"}
        xledger(("request", '{"id": "E-2", "lot": "L", "corpus_entries": [{"id": "2"}]}'),
                ("act", '{"id": "E-2", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry, new_entry, smuggled]}, f)
        xcommit()
        check("entree non revendiquee a cote d'une actee -> refusee",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Chemin hors surface : le canon est le territoire du juge.
        xreset()
        xledger(("request", '{"id": "E-4", "lot": "L", "paths": [".golden-master/canon/x.py"]}'),
                ("act", '{"id": "E-4", "lot": "L",'
                        ' "recorded_paths": [".golden-master/canon/x.py"]}'))
        os.makedirs(os.path.join(xgm, "canon"), exist_ok=True)
        with open(os.path.join(xgm, "canon", "x.py"), "w", encoding="utf-8") as f:
            f.write("# neuf\n")
        xcommit()
        check("chemin hors refs/+corpus (canon) -> refuse",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Registre reecrit : un registre COMMITTE a la base n'est plus un
        # prefixe de HEAD. (Un registre ne EXISTANT PAS a la base rend ce
        # check vacueux par construction — la, c'est « acte sans demande »
        # qui tient la ligne, teste plus bas.)
        xreset()
        xledger(("request", req2))
        xcommit()
        xbase2 = run("git rev-parse HEAD", xroot, timeout=60)[1].strip()
        xledger(("act", act2))  # la demande a disparu : trail edite
        with open(os.path.join(xgm, "refs", "2.txt"), "w", encoding="utf-8") as f:
            f.write("ref-2\n")
        xcommit()
        v = extension_verdict(xroot, ".golden-master", xbase2)
        check("registre committe puis reecrit -> append_only faux et acte refuse",
              [v["ledger_append_only"], v["acted"][0]["ok"]],
              [False, False])

        # Acte sans demande : il n'acte rien.
        xreset()
        xledger(("act", act2))
        with open(os.path.join(xgm, "refs", "2.txt"), "w", encoding="utf-8") as f:
            f.write("ref-2\n")
        xcommit()
        # Un acte introduit par le commit qui depose sa propre demande : le
        # lot a filed and answered, le subbot du filet n'a jamais juge.
        xreset()
        xledger(("request", req2), ("act", act2))
        with open(os.path.join(xgm, "refs", "2.txt"), "w", encoding="utf-8") as f:
            f.write("ref-2\n")
        xcommit()
        v_self = extension_verdict(xroot, ".golden-master", xbase)
        check("demande et acte dans UN commit (le lot s'auto-sert) -> refuse",
              [v_self["acted"][0]["ok"], v_self["ok_paths"]], [False, []])

        # Un acte qui n'enregistre que le registre : il ne laisse pas la
        # demande pendante, et il n'est pas certifie.
        xreset()
        ledger_only = ('{"id": "E-1", "lot": "L", "recorded_paths":'
                       ' [".golden-master/EXTENSIONS.md"]}')
        xrequest(req2)
        xledger(("request", req2), ("act", ledger_only))
        xcommit()
        check("acte n'enregistrant QUE le registre -> la demande RESTE pendante",
              [p["id"] for p in pending_extensions(xgm)], ["E-1"])
        check("acte n'enregistrant QUE le registre -> refuse",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"],
              False)

        # Un scalaire la ou le juge itere : le bloc devient refusable, il ne
        # fait plus tomber le verdict (qui etait lu comme "rien a refuser").
        xreset()
        scalar_act = '{"id": "E-1", "lot": "L", "recorded_paths": 1}'
        xrequest(req2)
        xledger(("request", req2), ("act", scalar_act))
        xcommit()
        check("recorded_paths scalaire -> la demande RESTE pendante, pas de crash",
              [p["id"] for p in pending_extensions(xgm)], ["E-1"])
        v_scalar = extension_verdict(xroot, ".golden-master", xbase)
        check("recorded_paths scalaire -> verdict rendu et acte refuse",
              ["error" not in v_scalar, v_scalar["acted"][0]["ok"]], [True, False])

        # Deux entrees d'assets aux prefixes DISTINCTS observent deux choses
        # differentes : refuser l'ajout serait un faux positif bloquant.
        a1 = {"id": "a1", "surface": "asset", "persona": "p",
              "static_prefix": "s/", "template_prefix": "t/"}
        a2 = {"id": "a2", "surface": "asset", "persona": "p",
              "static_prefix": "admin-s/", "template_prefix": "admin-t/"}
        check("deux entrees asset a prefixes distincts -> pas de collision",
              _entry_observation_key(a1) == _entry_observation_key(a2), False)

        check("acte sans demande -> refuse",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # 8f. Les refus arraches par la revue adversariale — chaque
        #     deguisement executee contre le verdict doit rester rouge.

        # Un acte VIDE ne ferme pas le terme de conjonction : quatre lignes
        # de JSON suffiraient a la partie contrainte pour eteindre la porte.
        xledger(("request", req2), ("act", '{"id": "E-1", "lot": "L"}'))
        check("acte sans recorded_paths -> la demande RESTE pendante",
              [p["id"] for p in pending_extensions(xgm)], ["E-1"])
        xledger(("request", req2),
                ("act", '{"id": "E-1", "lot": "L", "recorded_paths": []}'))
        check("acte a recorded_paths vide -> la demande RESTE pendante",
              [p["id"] for p in pending_extensions(xgm)], ["E-1"])

        # Un symlink sous refs/ : son contenu est sa cible, la prochaine
        # passe record ecrit A TRAVERS lui sur une reference existante.
        xreset()
        xledger(("request", req2), ("act", act2))
        os.symlink("1.txt", os.path.join(xgm, "refs", "2.txt"))
        xcommit()
        check("symlink sous refs/ -> refuse",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Un chemin refs/ enregistre mais non declare par la demande.
        xreset()
        xledger(("request", req2),
                ("act", '{"id": "E-1", "lot": "L",'
                        ' "recorded_paths": [".golden-master/refs/3.txt"]}'))
        with open(os.path.join(xgm, "refs", "3.txt"), "w", encoding="utf-8") as f:
            f.write("ref-3\n")
        xcommit()
        check("ref ajoutee non declaree par la demande -> refusee",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Un id duplique : l'egalite lit un jumeau, la capture sert l'autre.
        xreset()
        xledger(("request", '{"id": "E-2", "lot": "L", "corpus_entries": [{"id": "1"}]}'),
                ("act", '{"id": "E-2", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        twin = dict(base_entry, path="/detournee")
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [twin, base_entry]}, f)
        xcommit()
        check("id duplique dans le corpus -> refuse",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Une cle cosmetique ne neutralise pas la collision d'observation.
        xreset()
        cosmetic = dict(base_entry, id="9", note="differente en apparence")
        xledger(("request", '{"id": "E-3", "lot": "L", "corpus_entries": [{"id": "9"}]}'),
                ("act", '{"id": "E-3", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry, cosmetic]}, f)
        xcommit()
        check("collision masquee par une cle cosmetique -> refusee",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Une base irresoluble ne tamponne rien : elle refuse.
        check("GM_BASE irresoluble -> erreur, pas un laissez-passer",
              "error" in extension_verdict(xroot, ".golden-master", "d" * 40), True)

        # Un corpus malforme refuse, il ne crashe pas.
        xreset()
        xledger(("request", '{"id": "E-2", "lot": "L", "corpus_entries": [{"id": "2"}]}'),
                ("act", '{"id": "E-2", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            f.write('{"entries": {"E1": {}}}')
        xcommit()
        check("corpus malforme -> refuse, pas une traceback",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # 8g. Les refus du tour de CONSOLIDATION — la classe id-derive-un-
        #     chemin et la cle sensible a la presence, plus le flux add-entry
        #     legitime que le durcissement du tour 1 avait ferme.

        # Un id ajoute portant un separateur derive refs/<id>.txt HORS de
        # refs/ — le jumeau corpus du symlink.
        xreset()
        traversal = {"id": "../refs/1", "method": "GET", "path": "/t", "persona": "p"}
        xledger(("request", '{"id": "E-5", "lot": "L", "corpus_entries": [{"id": "../refs/1"}]}'),
                ("act", '{"id": "E-5", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry, traversal]}, f)
        xcommit()
        check("id en traversee (../refs/1) -> refuse",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # La PRESENCE d'un champ allowliste vide ne scinde pas la cle.
        xreset()
        present_twin = dict(base_entry, id="9", query="")
        xledger(("request", '{"id": "E-3", "lot": "L", "corpus_entries": [{"id": "9"}]}'),
                ("act", '{"id": "E-3", "lot": "L",'
                        ' "recorded_paths": [".golden-master/corpus.json"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry, present_twin]}, f)
        xcommit()
        check("collision masquee par un champ allowliste VIDE -> refusee",
              extension_verdict(xroot, ".golden-master", xbase)["acted"][0]["ok"], False)

        # Le flux add-entry LEGITIME : l'entree revendiquee revendique sa
        # reference derivee — corpus.json ET refs/2.txt exemptes, sans
        # `paths` dans la demande (la forme que la doctrine prescrit).
        xreset()
        xrequest('{"id": "E-2", "lot": "L", "type": "add-entry",'
                 ' "corpus_entries": [{"id": "2"}]}')
        xledger(("request", '{"id": "E-2", "lot": "L", "type": "add-entry",'
                            ' "corpus_entries": [{"id": "2"}]}'),
                ("act", '{"id": "E-2", "lot": "L", "recorded_paths":'
                        ' [".golden-master/corpus.json", ".golden-master/refs/2.txt"]}'))
        with open(os.path.join(xgm, "corpus.json"), "w", encoding="utf-8") as f:
            json.dump({"entries": [base_entry, new_entry]}, f)
        with open(os.path.join(xgm, "refs", "2.txt"), "w", encoding="utf-8") as f:
            f.write("ref-2\n")
        xcommit()
        v = extension_verdict(xroot, ".golden-master", xbase)
        check("add-entry avec sa ref derivee, sans `paths` -> ok, les deux exemptes",
              [v["acted"][0]["ok"], v["ok_paths"]],
              [True, [".golden-master/corpus.json", ".golden-master/refs/2.txt"]])

        # 9. Scellement : derivable partout, jamais dans le parent du worktree
        #    (lecture seule en sandbox), et sans collision entre worktrees
        #    freres qui partagent le meme basename.
        saved_env = {k: os.environ.get(k) for k in ("GM_SEALED_DIR", "GM_SCRATCH")}
        os.environ.pop("GM_SEALED_DIR", None)
        os.environ.pop("GM_SCRATCH", None)
        a, b = sealed_dir_for("/w/replay/app"), sealed_dir_for("/w/replay2/app")
        check("meme basename, deux worktrees -> deux piles", a != b, True)
        check("la pile ne vit pas dans le parent du worktree",
              a.startswith("/w/"), False)
        check("deux processus, meme workspace -> meme pile",
              sealed_dir_for("/w/replay/app") == a, True)
        os.environ["GM_SEALED_DIR"] = "/elsewhere/pile"
        check("GM_SEALED_DIR impose sa pile", sealed_dir_for("/w/x"), "/elsewhere/pile")
        for k, v in saved_env.items():
            os.environ.pop(k, None)
            if v is not None:
                os.environ[k] = v
        # ── Un mutant laisse APPLIQUE par une porte interrompue est reverti au
        # demarrage de la suivante. Les VRAIS apply/revert et empreintes, sur un
        # vrai depot : c'est le marqueur et le nettoyage qui sont juges, pas leur
        # doublure.
        g.update(apply_mutant=saved["apply_mutant"], revert_mutant=saved["revert_mutant"],
                 tree_fingerprint=saved["tree_fingerprint"], data_fingerprint=saved["data_fingerprint"])
        tmp = tempfile.mkdtemp(prefix="gm-leftover-")
        outside = tempfile.mkdtemp(prefix="gm-outside-")
        saved_scratch = os.environ.get("GM_SCRATCH")
        os.environ["GM_SCRATCH"] = tempfile.mkdtemp(prefix="gm-scratch-")
        try:
            def sub(*a):
                return subprocess.run(a, cwd=tmp, capture_output=True, text=True, check=True)
            sub("git", "init", "-q")
            sub("git", "config", "user.email", "t@t")
            sub("git", "config", "user.name", "t")
            with open(os.path.join(tmp, "f.txt"), "w", encoding="utf-8") as f:
                f.write("original\n")
            sub("git", "add", "f.txt")
            sub("git", "commit", "-qm", "seed")
            mdir = os.path.join(tmp, "m1")
            os.makedirs(mdir)

            def scripts(apply_body, revert_body):
                with open(os.path.join(mdir, "apply.sh"), "w", encoding="utf-8") as f:
                    f.write("#!/bin/sh\n" + apply_body + "\n")
                with open(os.path.join(mdir, "revert.sh"), "w", encoding="utf-8") as f:
                    f.write("#!/bin/sh\n" + revert_body + "\n")

            def tree():
                with open(os.path.join(tmp, "f.txt"), encoding="utf-8") as f:
                    return f.read()

            def clear_marker():
                # Tolerant on purpose: a mutant on the drop rules must fail as
                # CHECKS, not as an OSError that takes the whole self-test with it.
                try:
                    os.remove(applied_marker_for(tmp))
                except OSError:
                    pass
            scripts("printf 'mutant\\n' >> f.txt", "git checkout -- f.txt")
            lmeta = {"id": "leftover-1", "dir": mdir}
            code, _out = apply_mutant(lmeta, tmp)
            check("apply.sh tourne", code, 0)
            check("le marqueur d'application est pose", os.path.isfile(applied_marker_for(tmp)), True)
            check("le marqueur vit dans le git-dir de l'arbre, hors de ce qui est juge",
                  [applied_marker_for(tmp).startswith(os.path.realpath(os.path.join(tmp, ".git")) + os.sep),
                   "gm-applied" in sub("git", "status", "--porcelain").stdout],
                  [True, False])
            nogit = tempfile.mkdtemp(prefix="gm-nogit-")
            try:
                check("sans depot git, le marqueur retombe sous GM_SCRATCH",
                      applied_marker_for(nogit).startswith(os.environ["GM_SCRATCH"]), True)
            finally:
                shutil.rmtree(nogit, ignore_errors=True)
            # Interruption ici : pas de revert. La porte suivante nettoie.
            left = revert_leftover_mutant(tmp)
            check("le mutant laisse applique est identifie", (left or {}).get("id"), "leftover-1")
            check("l'arbre est revenu a HEAD", tree(), "original\n")
            check("le marqueur est efface apres un revert propre", os.path.isfile(applied_marker_for(tmp)), False)
            check("rien a revertir la seconde fois", revert_leftover_mutant(tmp), None)
            check("disposition d'un revert propre", leftover_disposition(left or {})[0], "reverted")
            # Le chemin nominal efface aussi le marqueur.
            apply_mutant(lmeta, tmp)
            revert_mutant(lmeta, tmp)
            check("le revert nominal efface le marqueur", os.path.isfile(applied_marker_for(tmp)), False)
            # Un revert qui ECHOUE garde le marqueur : la porte suivante le redira.
            scripts("printf 'mutant\\n' >> f.txt", "exit 3")
            apply_mutant(lmeta, tmp)
            left = revert_leftover_mutant(tmp)
            check("un revert en echec est signale avec son code", (left or {}).get("code"), 3)
            check("le marqueur reste tant que rien n'est reverti", os.path.isfile(applied_marker_for(tmp)), True)
            check("disposition d'un revert en echec : l'arbre est encore mute",
                  leftover_disposition(left or {})[0], "still_mutated")
            scripts("printf 'mutant\\n' >> f.txt", "git checkout -- f.txt")
            left = revert_leftover_mutant(tmp)
            check("le revert repare nettoie et efface", [tree(), os.path.isfile(applied_marker_for(tmp))],
                  ["original\n", False])
            # Un revert.sh disparu est signale, pas tu — et le marqueur reste.
            apply_mutant(lmeta, tmp)
            os.remove(os.path.join(mdir, "revert.sh"))
            left = revert_leftover_mutant(tmp)
            check("un mutant dont revert.sh a disparu est signale, pas tu",
                  [(left or {}).get("id"), left is not None and left.get("code") is None,
                   "revert.sh" in ((left or {}).get("out") or "")],
                  ["leftover-1", True, True])
            check("son marqueur reste", os.path.isfile(applied_marker_for(tmp)), True)
            check("disposition d'un revert.sh disparu", leftover_disposition(left or {})[0], "still_mutated")
            clear_marker()
            sub("git", "checkout", "--", "f.txt")
            # Une application qui ECHOUE nettoie l'arbre et le marqueur dans son propre run.
            scripts("printf 'half\\n' >> f.txt; exit 1", "git checkout -- f.txt")
            verdict, state = probe_mutation(dict(lmeta, targets=["001"]), tmp)
            check("une application en echec est un mutant 'failed'", state, "failed")
            check("elle a reverti l'arbre", tree(), "original\n")
            check("et efface son marqueur", os.path.isfile(applied_marker_for(tmp)), False)
            # LE SIGNAL D'ARRET EST L'ENREGISTREMENT, PAS `revert_clean`. Un
            # mutant INERTE — son point d'appui a disparu sous une modernisation
            # legitime, apply.sh ne mute plus rien — dont le revert.sh echoue
            # (`git checkout` d'un fichier que le lot a supprime) laisse un
            # enregistrement arme, et sa branche ne pose JAMAIS `revert_clean` :
            # la boucle continuait, chaque mutant suivant revenait INVALIDE en
            # nommant le coupable, et les comptes decrivaient un lot que
            # personne n'avait mesure.
            scripts("exit 0", "exit 1")
            v_inert, state = probe_mutation(dict(lmeta, targets=["001"]), tmp)
            check("un mutant qui ne mute rien est 'inert'", state, "inert")
            revert_mutant(lmeta, tmp)   # ce que fait la branche inerte de score_mutant
            check("son revert en echec laisse l'enregistrement arme",
                  [("revert_clean" in v_inert), leftover_on_record(tmp)], [False, True])
            check("et le classement S'ARRETE dessus, sans `revert_clean`",
                  scoring_must_stop(v_inert, tmp), True)
            check("l'application suivante est de toute facon refusee",
                  apply_mutant(lmeta, tmp)[0], APPLY_REFUSED)
            # LE SILENCE N'EST PAS UNE PREUVE DE PROPRETE. `revert_clean` n'est
            # ecrit que par le chemin complet ; les trois branches qui n'y
            # arrivent pas ne disent rien, et lire ce silence comme « propre »
            # rendait VERTE une porte sur un arbre que le harnais venait de
            # laisser mute, marqueur arme. Mesure : apply.sh mi-edite puis sort
            # 7, revert.sh sort 1 -> score 100 %, revert_clean vrai, held-out
            # 0/0, GREEN, `git status` = `M f.txt`.
            check("un mutant enregistre interdit d'annoncer un revert propre",
                  overall_revert_clean([{"revert_clean": True}], None, tmp), False)
            check("un classement arrete aussi, quoi que disent les verdicts",
                  overall_revert_clean([{"revert_clean": True}], {"id": "x"}, tmp), False)
            clear_marker()
            check("rien d'enregistre, un verdict propre : le classement continue",
                  [leftover_on_record(tmp), scoring_must_stop({"revert_clean": True}, tmp)],
                  [False, False])
            check("un revert sale arrete le classement meme sans enregistrement",
                  scoring_must_stop({"revert_clean": False}, tmp), True)
            # Une ECRITURE qui echoue ne laisse pas de coquille. O_CREAT a
            # reussi, l'ecriture non (racine scratch pleine) : le fichier vide
            # reste, et la porte suivante le lit comme un marqueur corrompu —
            # `unusable`, bail, toute application refusee — pour un mutant qui
            # n'a JAMAIS ete applique. Une racine pleine un instant coincait la
            # porte definitivement. RLIMIT_FSIZE=0 tient lieu d'ENOSPC.
            # Import local : `resource` est POSIX-seulement et seul l'autotest
            # s'en sert — en tete de fichier il ferait echouer l'IMPORT du
            # harnais la ou il manque, au lieu d'un seul controle.
            import resource
            soft, hard = resource.getrlimit(resource.RLIMIT_FSIZE)
            try:
                resource.setrlimit(resource.RLIMIT_FSIZE, (0, hard))
                ok_w, why_w = write_applied_marker(tmp, lmeta)
            finally:
                resource.setrlimit(resource.RLIMIT_FSIZE, (soft, hard))
            check("une ecriture de marqueur qui echoue refuse l'application",
                  [ok_w, "cannot write" in why_w], [False, True])
            check("et ne laisse aucune coquille derriere elle",
                  [os.path.exists(applied_marker_for(tmp)), leftover_on_record(tmp)],
                  [False, False])
            # Un marqueur dont les CHAMPS ne sont pas des chaines : `dir` part
            # directement dans realpath et os.path.join. Un nombre, une liste,
            # une chaine avec un NUL -> exception hors de main(), et le harnais
            # imprime une trace de pile la ou la campagne attend son unique
            # verdict JSON. Refuse comme un marqueur illisible, pas subi.
            os.makedirs(os.path.dirname(applied_marker_for(tmp)), mode=0o700, exist_ok=True)
            for label, payload in (("un nombre", '{"id": "x", "dir": 5}'),
                                   ("une liste", '{"id": "x", "dir": ["a"]}'),
                                   ("un NUL", '{"id": "x", "dir": "/a\\u0000b"}'),
                                   ("un id non-chaine", '{"id": 7, "dir": "%s"}' % mdir)):
                with open(applied_marker_for(tmp), "w", encoding="utf-8") as f:
                    f.write(payload)
                try:
                    got, raised = revert_leftover_mutant(tmp), ""
                except Exception as e:                  # noqa: BLE001 - c'est le test
                    got, raised = None, "%s: %s" % (type(e).__name__, e)
                check("marqueur dont un champ est %s : refuse, pas subi" % label,
                      [raised, (got or {}).get("refused"),
                       leftover_disposition(got or {})[0]], ["", "slot", "unusable"])
                clear_marker()
            check("arbre sain, verdicts sains : le revert est annonce propre",
                  overall_revert_clean([{"revert_clean": True}, {}], None, tmp), True)
            check("un seul verdict sale suffit a le nier",
                  overall_revert_clean([{"revert_clean": True}, {"revert_clean": False}],
                                       None, tmp), False)
            sub("git", "checkout", "--", "f.txt")
            # Seule la disposition « reverti » laisse passer la porte. Le lien entre
            # la decision et son consommateur est CETTE constante, pas trois lignes
            # de main() que rien ne conduit.
            check("seule une disposition 'reverted' laisse passer la porte",
                  [k in LEFTOVER_BAIL_KINDS
                   for k in ("still_mutated", "unusable", "refused", "reverted")],
                  [True, True, True, False])
            # Un marqueur qui nomme un repertoire que le harnais ne RECONNAIT pas :
            # rien n'est execute, et rien n'est efface non plus. Lire n'est pas
            # decider — le marqueur est pose AVANT apply.sh, donc son existence dit
            # que l'arbre est (ou peut etre) mute, quoi que ce repertoire designe
            # aujourd'hui. L'effacer detruisait la seule trace, et la porte
            # continuait sur un arbre d'etat inconnu : #799 qui recommence, en
            # silence.
            canary = os.path.join(outside, "ran")
            os.makedirs(os.path.join(outside, "evil"))
            with open(os.path.join(outside, "evil", "revert.sh"), "w", encoding="utf-8") as f:
                f.write("#!/bin/sh\ntouch %s\n" % shlex.quote(canary))
            write_applied_marker(tmp, {"id": "evil", "dir": os.path.join(outside, "evil")})
            left = revert_leftover_mutant(tmp)
            check("un marqueur etranger est refuse, rien n'est execute",
                  [(left or {}).get("refused"), os.path.exists(canary)], ["dir", False])
            check("le marqueur etranger est GARDE : c'est la seule trace de l'application",
                  os.path.isfile(applied_marker_for(tmp)), True)
            check("disposition d'un marqueur etranger", leftover_disposition(left or {})[0], "refused")
            check("elle arrete la porte", leftover_disposition(left or {})[0] in LEFTOVER_BAIL_KINDS, True)
            check("et son message nomme le marqueur a effacer a la main",
                  applied_marker_for(tmp) in leftover_disposition(left or {})[1], True)
            # Le refus COLLE : la porte suivante refuse a l'identique, et aucune
            # application ne passe tant que le marqueur est la.
            again = revert_leftover_mutant(tmp)
            check("la porte suivante refuse a l'identique, canari toujours pas execute",
                  [(again or {}).get("refused"), os.path.exists(canary),
                   os.path.isfile(applied_marker_for(tmp))], ["dir", False, True])
            scripts("printf 'mutant\\n' >> f.txt", "git checkout -- f.txt")
            code, _out = apply_mutant(lmeta, tmp)
            check("aucune application ne passe tant qu'un refus est enregistre",
                  [code, tree()], [APPLY_REFUSED, "original\n"])
            clear_marker()
            # Un marqueur sous la racine scratch mais HORS de la pile scellee est
            # etranger aussi : la racine, c'est tout /tmp sur un hote partage.
            evil2 = os.path.join(os.environ["GM_SCRATCH"], "evil2")
            os.makedirs(evil2)
            with open(os.path.join(evil2, "revert.sh"), "w", encoding="utf-8") as f:
                f.write("#!/bin/sh\ntouch %s\n" % shlex.quote(canary))
            write_applied_marker(tmp, {"id": "evil2", "dir": evil2})
            left = revert_leftover_mutant(tmp)
            check("un marqueur sous la racine scratch, hors pile scellee, est refuse et garde",
                  [(left or {}).get("refused"), os.path.exists(canary),
                   os.path.isfile(applied_marker_for(tmp))], ["dir", False, True])
            clear_marker()
            # Un marqueur sous la pile scellee (GM_SEALED_DIR, hors arbre et hors
            # scratch) est le held-out : il est reverti, pas refuse.
            sealed = tempfile.mkdtemp(prefix="gm-sealed-")
            saved_sealed = os.environ.get("GM_SEALED_DIR")
            os.environ["GM_SEALED_DIR"] = sealed
            try:
                hdir = os.path.join(sealed, "h1")
                os.makedirs(hdir)
                with open(os.path.join(hdir, "apply.sh"), "w", encoding="utf-8") as f:
                    f.write("#!/bin/sh\nprintf 'held\\n' >> f.txt\n")
                with open(os.path.join(hdir, "revert.sh"), "w", encoding="utf-8") as f:
                    f.write("#!/bin/sh\ngit checkout -- f.txt\n")
                code, _out = apply_mutant({"id": "h1", "dir": hdir}, tmp)
                check("le held-out scelle s'applique", [code, tree()], [0, "original\nheld\n"])
                left = revert_leftover_mutant(tmp)
                check("un marqueur sous la pile scellee est reverti, pas refuse",
                      [(left or {}).get("refused"), (left or {}).get("code"), tree()],
                      [False, 0, "original\n"])
                check("disposition du held-out reverti", leftover_disposition(left or {})[0], "reverted")
                # LE CAS MESURE. La pile scellee a bouge entre deux passes —
                # GM_SEALED_DIR pose a l'application, absent a la porte suivante :
                # exactement la configuration que le harnais recommande quand la
                # racine par defaut n'est pas stable. Le held-out laisse applique
                # se lit alors comme etranger. Tant que ce refus jetait le
                # marqueur, la porte notait et CONTINUAIT sur l'arbre mute, sans
                # rien pour le dire a la suivante.
                code, _out = apply_mutant({"id": "h1", "dir": hdir}, tmp)
                check("le held-out est re-applique", [code, tree()], [0, "original\nheld\n"])
                os.environ.pop("GM_SEALED_DIR", None)
                moved = revert_leftover_mutant(tmp)
                check("pile scellee deplacee : refus, marqueur garde, arbre encore mute",
                      [(moved or {}).get("refused"), os.path.isfile(applied_marker_for(tmp)), tree()],
                      ["dir", True, "original\nheld\n"])
                check("et la porte s'arrete au lieu de juger cet arbre",
                      leftover_disposition(moved or {})[0] in LEFTOVER_BAIL_KINDS, True)
                os.environ["GM_SEALED_DIR"] = sealed
                healed = revert_leftover_mutant(tmp)
                check("la pile scellee retrouvee, le harnais revertit lui-meme",
                      [(healed or {}).get("code"), tree(),
                       os.path.isfile(applied_marker_for(tmp))], [0, "original\n", False])
            finally:
                os.environ.pop("GM_SEALED_DIR", None)
                if saved_sealed is not None:
                    os.environ["GM_SEALED_DIR"] = saved_sealed
                shutil.rmtree(sealed, ignore_errors=True)
            # Un `git` hors d'atteinte doit donner un VERDICT, pas une trace de
            # pile : ce balayage tourne dans TOUS les modes, `record` compris, ou
            # rien d'autre n'appelle git — et la campagne lit du JSON sur la
            # sortie standard. PATH reduit a `sh` : git est absent, le script de
            # revert (un `exit 0`, une primitive du shell) tourne encore.
            scripts("exit 0", "exit 0")
            apply_mutant(lmeta, tmp)
            saved_path, shbin = os.environ.get("PATH", ""), shutil.which("sh")
            check("`sh` est sur le PATH (tout le harnais passe par lui)", bool(shbin), True)
            bindir = tempfile.mkdtemp(prefix="gm-bin-")
            try:
                os.symlink(shbin or "/bin/sh", os.path.join(bindir, "sh"))
                os.environ["PATH"] = bindir
                try:
                    nogit = revert_leftover_mutant(tmp)
                    raised = ""
                except Exception as e:                      # noqa: BLE001 - c'est le test
                    nogit, raised = None, "%s: %s" % (type(e).__name__, e)
            finally:
                os.environ["PATH"] = saved_path
                shutil.rmtree(bindir, ignore_errors=True)
            check("git absent : un verdict, pas une exception",
                  [raised, (nogit or {}).get("code")], ["", 0])
            check("l'etat de l'arbre est INCONNU, pas propre",
                  [(nogit or {}).get("dirty"), bool((nogit or {}).get("dirty_unknown"))],
                  ["", True])
            check("et la note le dit au lieu de laisser croire a un arbre propre",
                  "Unknown, not clean" in leftover_disposition(nogit or {})[1], True)
            clear_marker()
            # Un revert en ECHEC garde son enregistrement face au mutant suivant :
            # la seconde application est refusee, rien ne tourne, et le revert du
            # second n'efface pas le marqueur du premier.
            scripts("printf 'mutant\\n' >> f.txt", "exit 3")
            code, _out = apply_mutant(lmeta, tmp)
            check("A s'applique", code, 0)
            code, out = revert_mutant(lmeta, tmp)
            check("le revert de A echoue et A reste enregistre",
                  [code, (read_applied_marker(tmp)[0] or {}).get("id")], [3, "leftover-1"])
            bdir = os.path.join(tmp, "m2")
            os.makedirs(bdir)
            with open(os.path.join(bdir, "apply.sh"), "w", encoding="utf-8") as f:
                f.write("#!/bin/sh\nprintf 'B\\n' >> f.txt\n")
            with open(os.path.join(bdir, "revert.sh"), "w", encoding="utf-8") as f:
                f.write("#!/bin/sh\ngit checkout -- f.txt\n")
            bmeta = {"id": "leftover-2", "dir": bdir}
            code, out = apply_mutant(bmeta, tmp)
            check("l'application de B est refusee tant que A est enregistre",
                  [code, "leftover-1" in out, tree()], [APPLY_REFUSED, True, "original\nmutant\n"])
            verdict, state = probe_mutation(dict(bmeta, targets=["001"]), tmp)
            check("la sonde de B est 'failed' sans rien reverter ni toucher au marqueur",
                  [state, tree(), (read_applied_marker(tmp)[0] or {}).get("id")],
                  ["failed", "original\nmutant\n", "leftover-1"])
            code, _out = revert_mutant(bmeta, tmp)
            check("le revert de B (exit 0) n'efface pas l'enregistrement de A",
                  [code, (read_applied_marker(tmp)[0] or {}).get("id")], [0, "leftover-1"])
            clear_marker()
            sub("git", "checkout", "--", "f.txt")
            # Un repertoire de marqueur qui n'est pas prive n'est pas utilisable :
            # l'application est refusee, la porte s'arrete (disposition 'unusable').
            mdir_marker = os.path.dirname(applied_marker_for(tmp))
            scripts("printf 'mutant\\n' >> f.txt", "git checkout -- f.txt")
            os.chmod(mdir_marker, 0o755)
            try:
                code, out = apply_mutant(lmeta, tmp)
                check("un repertoire de marqueur non prive refuse l'application",
                      [code, "not private" in out, tree()], [APPLY_REFUSED, True, "original\n"])
                os.chmod(mdir_marker, 0o700)
                apply_mutant(lmeta, tmp)
                os.chmod(mdir_marker, 0o755)
                left = revert_leftover_mutant(tmp)
                check("un marqueur dans un repertoire non prive est refuse et garde, rien n'est execute",
                      [(left or {}).get("refused"), (left or {}).get("kept"), tree(),
                       os.path.isfile(applied_marker_for(tmp))],
                      ["slot", True, "original\nmutant\n", True])
                check("disposition d'un emplacement inutilisable", leftover_disposition(left or {})[0], "unusable")
            finally:
                os.chmod(mdir_marker, 0o700)
            left = revert_leftover_mutant(tmp)
            check("le meme marqueur, l'emplacement redevenu prive : reverti",
                  [(left or {}).get("code"), tree()], [0, "original\n"])
            # ── Ajouts : racines resserrees sur le repertoire du filet, sentinelle
            # qui n'est pas un code de retour. Etat remis a plat d'abord.
            try:
                os.remove(applied_marker_for(tmp))
            except OSError:
                pass
            sub("git", "checkout", "--", "f.txt")
            scripts("printf 'mutant\\n' >> f.txt", "git checkout -- f.txt")
            apply_mutant(lmeta, tmp)
            left = revert_leftover_mutant(tmp, os.path.join(tmp, ".golden-master"))
            check("hors du repertoire du filet nomme, le marqueur est refuse et garde",
                  [(left or {}).get("refused"), tree(), os.path.isfile(applied_marker_for(tmp))],
                  ["dir", "original\nmutant\n", True])
            left = revert_leftover_mutant(tmp)
            check("sans repertoire de filet nomme, l'arbre entier est reconnu : reverti",
                  [(left or {}).get("code"), tree()], [0, "original\n"])
            # Une application TUEE par un signal (returncode -1, ce que rapporte
            # subprocess pour un shell mort sur SIGHUP) n'est pas une application
            # refusee : elle a tourne, elle est revertie dans le run. Le -1 est
            # injecte par une doublure de run_script (un signal reel depend du
            # shell : exec ou non de la derniere commande).
            scripts("printf 'half\\n' >> f.txt", "git checkout -- f.txt")
            real_run_script = g["run_script"]

            def killed_apply(path, ws, timeout=600):
                if path.endswith("apply.sh"):
                    real_run_script(path, ws, timeout)
                    return -1, "killed by SIGHUP"
                return real_run_script(path, ws, timeout)
            g["run_script"] = killed_apply
            try:
                verdict, state = probe_mutation(dict(lmeta, targets=["001"]), tmp)
            finally:
                g["run_script"] = real_run_script
            check("une application tuee par un signal est revertie, pas lue comme refusee",
                  [state, tree(), os.path.isfile(applied_marker_for(tmp))],
                  ["failed", "original\n", False])
            # ── Le residu d'un revert qui dit « 0 » sans tout restaurer : ce qui
            # est encore modifie ET etait propre quand le mutant est descendu.
            try:
                os.remove(applied_marker_for(tmp))
            except OSError:
                pass
            sub("git", "checkout", "--", "f.txt")
            scripts("printf 'mutant\\n' >> f.txt", "true")
            apply_mutant(lmeta, tmp)
            db = (read_applied_marker(tmp)[0] or {}).get("dirty_before")
            check("le marqueur enregistre ce qui etait deja modifie avant l'application (pas f.txt)",
                  [isinstance(db, list), "f.txt" in (db or [])], [True, False])
            left = revert_leftover_mutant(tmp)
            check("un revert.sh a 0 qui ne restaure pas laisse un residu nomme, marqueur garde",
                  [(left or {}).get("code"), (left or {}).get("residue"), (left or {}).get("kept"),
                   os.path.isfile(applied_marker_for(tmp))],
                  [0, ["f.txt"], True, True])
            check("disposition d'un residu : la porte s'arrete",
                  [leftover_disposition(left or {})[0], leftover_disposition(left or {})[0] in LEFTOVER_BAIL_KINDS],
                  ["still_mutated", True])
            os.remove(applied_marker_for(tmp))
            sub("git", "checkout", "--", "f.txt")
            # Le travail non committe d'AVANT le mutant n'est pas un residu.
            with open(os.path.join(tmp, "g.txt"), "w", encoding="utf-8") as f:
                f.write("operator work\n")
            scripts("printf 'mutant\\n' >> f.txt", "git checkout -- f.txt")
            apply_mutant(lmeta, tmp)
            check("le travail deja non committe est enregistre comme tel",
                  "g.txt" in ((read_applied_marker(tmp)[0] or {}).get("dirty_before") or []), True)
            left = revert_leftover_mutant(tmp)
            check("un revert propre au milieu du travail de l'operateur : pas de residu, note, marqueur efface",
                  [(left or {}).get("residue"), "g.txt" in ((left or {}).get("dirty") or ""),
                   leftover_disposition(left or {})[0], os.path.isfile(applied_marker_for(tmp))],
                  [[], True, "reverted", False])
            os.remove(os.path.join(tmp, "g.txt"))
            # Un marqueur SANS dirty_before (git muet a l'application, ou harnais
            # d'avant) et un arbre encore modifie apres le revert : INDECIDABLE,
            # donc pas propre — marqueur garde, porte arretee. Arbre propre : reverti.
            scripts("printf 'mutant\\n' >> f.txt", "true")
            apply_mutant(lmeta, tmp)
            mpath = applied_marker_for(tmp)
            with open(mpath, "w", encoding="utf-8") as f:
                json.dump({"id": "leftover-1", "dir": mdir}, f)
            left = revert_leftover_mutant(tmp)
            check("sans dirty_before, un arbre encore modifie est indecidable : still_mutated, marqueur garde",
                  [(left or {}).get("residue_undecided"), (left or {}).get("residue"),
                   leftover_disposition(left or {})[0], os.path.isfile(applied_marker_for(tmp))],
                  [True, ["f.txt"], "still_mutated", True])
            os.remove(applied_marker_for(tmp))
            sub("git", "checkout", "--", "f.txt")
            # Un arbre VRAIMENT propre : les repertoires de mutants de ce test sont
            # des fichiers non suivis, donc « modifies » pour git — ignores ici.
            with open(os.path.join(tmp, ".gitignore"), "w", encoding="utf-8") as f:
                f.write("m1/\nm2/\n")
            sub("git", "add", ".gitignore")
            sub("git", "commit", "-qm", "ignore the mutant dirs")
            scripts("printf 'mutant\\n' >> f.txt", "git checkout -- f.txt")
            apply_mutant(lmeta, tmp)
            with open(mpath, "w", encoding="utf-8") as f:
                json.dump({"id": "leftover-1", "dir": mdir}, f)
            left = revert_leftover_mutant(tmp)
            check("sans dirty_before, un arbre propre apres le revert est reverti",
                  [(left or {}).get("residue"), leftover_disposition(left or {})[0], os.path.isfile(applied_marker_for(tmp))],
                  [[], "reverted", False])
            # Un dirty_before qui n'est pas une liste de chemins est refuse a la lecture.
            mpath = applied_marker_for(tmp)
            os.makedirs(os.path.dirname(mpath), mode=0o700, exist_ok=True)
            with open(mpath, "w", encoding="utf-8") as f:
                json.dump({"id": "x", "dir": mdir, "dirty_before": 5}, f)
            os.chmod(mpath, 0o600)
            check("un dirty_before qui n'est pas une liste est refuse",
                  bool(read_applied_marker(tmp)[1]), True)
            os.remove(mpath)
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
            shutil.rmtree(outside, ignore_errors=True)
            shutil.rmtree(os.environ["GM_SCRATCH"], ignore_errors=True)
            if saved_scratch is None:
                os.environ.pop("GM_SCRATCH", None)
            else:
                os.environ["GM_SCRATCH"] = saved_scratch
    finally:
        g.update(saved)

    if failures:
        log("harnais : %d test(s) ECHOUENT" % len(failures))
        for f in failures:
            log("  " + f)
        return 1
    log("harnais : %d verifications passent" % checked[0])
    return 0


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
    mode = os.environ.get("GM_MODE", "gate")   # gate | record | selfcheck | validate | selftest | extensions | extend-verify

    # Les tests du harnais d'abord, et hors de tout le reste : ils ne touchent
    # ni au depot, ni a l'application, ni a la configuration.
    if mode == "selftest":
        raise SystemExit(_selftest())

    # Ledger listings and the additions-only verdict are pure git/file reads:
    # no boot, no capture, callable from a verifier that must not pay for
    # either — and BY the party whose own work they judge, because the code
    # deciding is this file's, not theirs.
    if mode == "extensions":
        print(json.dumps({"pending": pending_extensions(gm_dir)}))
        raise SystemExit(0)
    if mode == "extend-verify":
        base = os.environ.get("GM_BASE", "")
        if not base:
            print(json.dumps({"error": "GM_BASE is required — judging "
                              "additions against nothing would pass by "
                              "construction"}))
            raise SystemExit(1)
        print(json.dumps(extension_verdict(
            ws, os.environ.get("GM_DIR", ".golden-master"), base)))
        raise SystemExit(0)

    report = {"mode": mode, "total": 0, "valid": 0, "detected": 0, "score_pct": 0,
              "noop_silent": False, "revert_clean": True, "collateral": 0,
              "unstable_controls": [],
              "notice": "", "uncontrolled": [], "blind_lanes": [], "missing_archetypes": [],
              "missing_corpus_probes": [], "uncovered_routes": [],
              "routes_total": 0, "routes_excluded": 0,
              "standard": 2, "unmapped_features": [], "stale_features": [],
              "features_total": 0, "features_excluded": 0,
              "holdout_awaiting_gate": False,
              "pending_rebaselines": [],
              "pending_extensions": [],
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

    # The net's declared standard. 2 = routes-era nets; 3 adds the feature
    # inventory. A net keeps its declared standard until its owner raises it
    # through the ledger — and the verdict CARRIES the figure, so a net below
    # the current standard is visible, never silent.
    try:
        std = int(config.get("standard", 2))
    except (TypeError, ValueError):
        bail("config `standard` is not an integer: %r" % (config.get("standard"),))
    report["standard"] = std

    # A raise is a one-way ratchet — see standard_mark_verdict for why both
    # directions of config/mark drift are refusals.
    level, msg = standard_mark_verdict(gm_dir, std)
    if level == "bail":
        bail(msg)
    elif level == "note":
        note(report, msg)
    if std < 3 and config.get("feature_probe"):
        note(report, "config declares a feature_probe but standard %d does not "
                     "consume it — raise to standard 3 or remove the probe; a "
                     "probe nobody runs reads as coverage." % std)

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
    # IDEMPOTENT: the standalone and the inlined node differ only by their
    # header, so the copy is normalised to ONE canonical form and written only
    # when it differs. A gate that dirties the tree with its own header churn
    # leaves the run un-mergeable and forces a human to land it by hand.
    try:
        with open(__file__, encoding="utf-8") as f:
            src = f.read()
        body = src[src.index("\nimport hashlib"):]
        canon_copy = (
            "#!/usr/bin/env python3\n"
            '"""Materialised oracle harness — the decision procedure, not the '
            "campaign's to edit.\n"
            "The reviewable source of truth lives in the golden-master bot "
            "bundle; this copy\n"
            "exists so the emitted runner, CI and later passes judge with the "
            "same code.\n"
            'Regenerated at every gate; edits made here do not survive."""\n'
            + body)
        target = os.path.join(gm_dir, "harness.py")
        old = ""
        if os.path.isfile(target):
            with open(target, encoding="utf-8") as f:
                old = f.read()
        if old != canon_copy:
            with open(target, "w", encoding="utf-8") as f:
                f.write(canon_copy)
    except (OSError, NameError, ValueError):
        pass

    # Une porte sur un arbre sale juge un arbre qui n'a jamais existé : les
    # captures partent de ce qui est là, puis le premier revert de mutant —
    # `git checkout -- <fichier>` — ramène ces fichiers à HEAD, et tout ce qui
    # est capturé ensuite décrit autre chose. Le travail non committé est
    # détruit au passage, en silence. Signalé plutôt que refusé : enregistrer et
    # itérer sur un arbre sale est légitime, GATER dessus ne l'est pas.
    # A mutant left APPLIED by an interrupted gate is reverted BEFORE the tree
    # is looked at, and said: otherwise the dirty check below names the
    # mutant's file as the lot's uncommitted work, and the build gate judges a
    # program nobody wrote.
    left = revert_leftover_mutant(ws, gm_dir)
    if left:
        kind, text = leftover_disposition(left)
        if kind in LEFTOVER_BAIL_KINDS:
            bail(text)
        note(report, text)

    if mode != "record":
        dirty = subprocess.run(["git", "-C", ws, "--no-optional-locks", "status", "--porcelain"],
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
            note(report, (
                "WORKSPACE NOT COMMITTED (%d path(s): %s). Mutant reverts restore HEAD, so "
                "these changes are destroyed during the run and the verdict below describes "
                "a tree that never existed. Commit, then gate."
                % (len(paths), ", ".join(sorted(paths)[:12]))))

    visible = load_mutants(gm_dir, holdout=False)

    # MODE=validate — la validite MECANIQUE seule, sans application ni capture.
    #
    # Pourquoi ce mode existe : un lot de modernisation a le droit de supprimer
    # le point d'appui d'un mutant (renommer une methode, changer un gabarit).
    # Le mutant devient alors INVALIDE — il n'a plus rien a muter — et le filet
    # perd en silence la surface qu'il couvrait. Le reparer est mecanique : on
    # le re-ancre. L'accepter ne l'est pas, et c'est la que ce mode sert : il
    # etablit qu'un mutant re-ancre MUTE de nouveau quelque chose, par le code
    # meme qui en decidera a la porte, en quelques secondes plutot qu'en
    # vingt-cinq minutes.
    #
    # CE QU'IL N'ETABLIT PAS, et la distinction est tout : que le filet VOIE le
    # mutant. Voir se prouve en capturant et en comparant, ce que seule la porte
    # fait. Un mode qui repondrait « valide » et se laisserait lire comme « la
    # porte passera » serait precisement le controle qui annonce une chose et en
    # etablit une autre.
    if mode == "validate":
        wanted = [i.strip() for i in os.environ.get("GM_MUTANTS", "").split(",") if i.strip()]
        chosen = [m for m in visible if not wanted or m["id"] in wanted]
        report["missing"] = sorted(set(wanted) - {m["id"] for m in chosen})
        verdicts, stopped_at = [], None
        for m in chosen:
            v, state = probe_mutation(m, ws)
            if state != "failed":
                revert_mutant(m, ws)
            verdicts.append(v)
            # The gate's rule, here too. One mutant whose revert failed leaves
            # the record armed; without this, every LATER mutant came back
            # invalid quoting that one, while the tail still announced them all
            # "applied, fingerprinted and reverted" and the tree stayed mutated.
            # This is the mode the campaign uses to accept a RE-ANCHORED mutant,
            # so the cascade sent it to repair mutants that were fine.
            if leftover_on_record(ws):
                stopped_at = v
                break
        report["total"] = len(verdicts)
        report["valid"] = len([v for v in verdicts if v.get("valid")])
        report["invalid"] = [{"id": v["id"], "reason": v.get("reason", "")}
                             for v in verdicts if not v.get("valid")]
        done = len(verdicts) - (1 if stopped_at is not None else 0)
        report["log_tail"] = (
            "MODE=validate — %d mutant(s) applied, fingerprinted and reverted. This says "
            "each one CHANGES something; it says NOTHING about whether the net sees it. "
            "Only the gate captures and compares, and the zeroed fields above are defaults, "
            "not a verdict." % done)
        if stopped_at is not None:
            report["log_tail"] += (
                " STOPPED at mutant %s: it is still recorded as applied, so the tree is not "
                "at baseline and the %d mutant(s) after it were NOT validated — every "
                "application there would be refused, not measured. Revert by hand, delete "
                "the marker %s, then re-run."
                % (stopped_at.get("id"), len(chosen) - len(verdicts), applied_marker_for(ws)))
        if report["missing"]:
            report["log_tail"] += (" %d requested mutant(s) do not exist: %s."
                                   % (len(report["missing"]), ", ".join(report["missing"])))
        print(json.dumps(report))
        return

    if mode != "record":
        try:
            seal_holdout(gm_dir, sealed_dir)
            awaiting = holdout_committed_in_tree(gm_dir) and \
                not seal_committed_opted_in(gm_dir)
        except SystemExit as e:
            bail(str(e))
        if awaiting:
            # Machine-readable, not only prose: a set nobody ever consumes is
            # a debt of the NET's owner, and a supervising process needs a
            # field to see it — a notice string is where debts go to hide.
            report["holdout_awaiting_gate"] = True
            note(report, "a fresh held-out set is COMMITTED under mutants/holdout/ "
                         "and awaits its own convergence gate: this gate leaves it "
                         "in place and scores without it. `\"seal_committed\": true` "
                         "in config.json (or GM_SEAL_COMMITTED=1) is that gate's "
                         "explicit opt-in — and the owner's next campaign MUST "
                         "consume the set; a committed set with no gate on its "
                         "horizon narrows the counter-test while reading as "
                         "prepared.")
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
                note(report, "the held-out set for this cycle is SPENT and published "
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

        probe_gaps = missing_corpus_probes(corpus, config)
        if probe_gaps:
            report["missing_corpus_probes"] = probe_gaps
            bail("the corpus is missing required probes: %s. A mutant can only test "
                 "what the corpus watches; every probe here is a regression class "
                 "that shipped through a corpus hole once. See "
                 "skills/surface-discovery.md."
                 % json.dumps(probe_gaps, ensure_ascii=False))

        # The perimeter check runs on the TREE, before the application boots:
        # an unwatched route is a fact of the corpus, not of the run.
        if not config.get("routes_probe"):
            bail("config declares no `routes_probe`: the net cannot state the "
                 "perimeter it claims to defend. Write one next to `state_probe` — "
                 "one route per line, printed from the target's own routing "
                 "declarations — and justify every deliberate hole in "
                 "route-coverage.json.")
        try:
            uncovered, r_total, r_excl = route_coverage(gm_dir, config, corpus, ws)
        except SystemExit as e:
            bail(str(e))
        report["routes_total"], report["routes_excluded"] = r_total, r_excl
        if uncovered:
            report["uncovered_routes"] = uncovered
            bail("%d route(s) the corpus never touches: %s%s. Cover each one, or write "
                 "its exclusion WITH ITS REASON in route-coverage.json — an unwatched "
                 "route is exactly where the last regression class shipped."
                 % (len(uncovered), ", ".join(uncovered[:20]),
                    ", …" if len(uncovered) > 20 else ""))

        pending = pending_rebaselines(gm_dir)
        if pending:
            report["pending_rebaselines"] = pending
            bail("the ledger carries %d pending re-baseline request(s): %s. Each one "
                 "quarantines known-diverging entries out of this verdict — a green "
                 "built around them narrows the net while reporting progress. Act "
                 "them (record, diff == announced, act block, then the verdict after "
                 "a green counter-test), or refuse them in writing."
                 % (len(pending), json.dumps(pending, ensure_ascii=False)))

        pending_ext = pending_extensions(gm_dir)
        if pending_ext:
            report["pending_extensions"] = pending_ext
            bail("the ledger carries %d pending extension request(s): %s. Each one "
                 "names an observation the net was asked to gain and does not "
                 "have — a green built while they wait reports coverage the "
                 "intent already knows is missing. The net's extension subbot "
                 "acts them; what it cannot apply additively goes back to its "
                 "requester in writing."
                 % (len(pending_ext), json.dumps(pending_ext, ensure_ascii=False)))

    try:
        app_up(config, ws)
    except SystemExit as e:
        bail(str(e))

    # The feature perimeter is judged against the LIVE application — the probe
    # may walk the served navigation — so it sits after boot, unlike routes.
    # It binds from standard 3; record mode is exempt because recording is how
    # a net under construction earns its inventory.
    if mode != "record" and std >= 3:
        if not config.get("feature_probe"):
            bail("config declares standard %d but no `feature_probe`: the net "
                 "cannot state the features it claims to cover. Write one next "
                 "to `routes_probe` — `<feature-id> <source>` per line, from at "
                 "least two independent sources (the served navigation graph, "
                 "the tree's message/template catalogue) — and map each feature "
                 "to corpus entries in feature-coverage.json." % std)
        try:
            f_unmapped, f_stale, f_total, f_excl = feature_coverage(
                gm_dir, config, corpus, ws)
        except SystemExit as e:
            bail(str(e))
        report["features_total"], report["features_excluded"] = f_total, f_excl
        if f_stale:
            report["stale_features"] = f_stale
            bail("%d feature(s) in feature-coverage.json the probe no longer "
                 "prints: %s%s. A map to a surface that is gone reads as "
                 "coverage — prune the inventory or fix the probe."
                 % (len(f_stale), ", ".join(f_stale[:20]),
                    ", …" if len(f_stale) > 20 else ""))
        if f_unmapped:
            report["unmapped_features"] = f_unmapped
            bail("%d feature(s) the corpus never exercises: %s%s. Map each one "
                 "to the entries that exercise it, or write its exclusion WITH "
                 "ITS REASON in feature-coverage.json — a route touched once is "
                 "not a feature covered."
                 % (len(f_unmapped), ", ".join(f_unmapped[:20]),
                    ", …" if len(f_unmapped) > 20 else ""))

    try:
        if mode == "record":
            # GM_RECORD_IDS scopes the record to named entries — the extension
            # subbot needs to capture EXACTLY the entries it added: a full
            # re-record inside a lot where behaviour legitimately moved would
            # rewrite every reference and be refused wholesale as smuggling.
            only = [i for i in os.environ.get("GM_RECORD_IDS", "").split(",")
                    if i.strip()]
            if only:
                known = {e["id"] for e in corpus["entries"]}
                missing = [i for i in only if i not in known]
                if missing:
                    bail("GM_RECORD_IDS names %d entr%s absent from the "
                         "corpus: %s — recording an unknown id writes a "
                         "reference nothing will ever compare"
                         % (len(missing), "y" if len(missing) == 1 else "ies",
                            ", ".join(missing)))
            snap = capture(config, corpus, canon, ids=only or None)
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

        # GM_MUTANTS en mode porte — restreindre le PARCOURS, jamais le verdict.
        #
        # Rejouer un mutant seul coute une montee d'application au lieu d'un
        # cycle complet, ce qui rend une hypothese testable en minutes. Deux
        # precautions, et elles sont ce qui rend la restriction acceptable :
        # la graine reste l'index d'ORIGINE, sinon l'echantillon de controle
        # change et le mutant n'est plus juge sur les memes temoins ; et le
        # rapport sort sous `gate-subset`, que le lanceur refuse de lire comme
        # un verdict. Une porte verte obtenue en ne jouant qu'un mutant serait
        # la facon la plus economique de mentir de tout ce dispositif.
        only = {i.strip() for i in os.environ.get("GM_MUTANTS", "").split(",") if i.strip()}
        if only:
            report["mode"] = "gate-subset"
            note(report, "GM_MUTANTS restricts this run to %s. The archetype and corpus "
                         "checks still ran on the WHOLE set, but the score, the collateral "
                         "and the blind lanes below describe a SUBSET and are not a gate."
                         % ", ".join(sorted(only)))

        verdicts, blind = [], []
        # A revert that did not restore the baseline ends the scoring: every
        # later measurement would describe a program nobody wrote, and every
        # later apply is refused anyway while the leftover is on record.
        #
        # TWO signals, because one does not cover the rule. `revert_clean` is
        # written only on the full apply→capture→revert path — revert.sh
        # exited non-zero, or the captures still differ with the mutant gone.
        # The inert branch and the failed-apply branch also run a revert and
        # also ignore its exit code, and a leftover armed there used to let the
        # loop run on: every remaining mutant came back INVALID naming the
        # culprit, the counts described a lot nobody measured, and the operator
        # was told to revert a tree by hand. `leftover_on_record` is what all
        # three leave behind. The gate is red either way; stopping keeps it
        # legible.
        stopped = None
        for seed, meta in enumerate(visible):
            if only and meta["id"] not in only:
                continue
            v = score_mutant(meta, config, corpus, canon, refs, ws, seed)
            verdicts.append(v)
            if v.get("valid"):
                if not v.get("detected"):
                    blind.append({"surface": v.get("surface"), "archetype": v.get("archetype"),
                                  "mutant_id": v["id"], "entries": v.get("targets_declared", []),
                                  "why": "no declared target moved"})
                elif v.get("undetected_targets"):
                    blind.append({"surface": v.get("surface"), "archetype": v.get("archetype"),
                                  "mutant_id": v["id"], "entries": v["undetected_targets"],
                                  "why": "these references did not move for a change they cover"})
            # Classified FIRST, then the stop. A verdict whose revert failed
            # still reached capture and comparison, so what it says about the
            # net is evidence; breaking before this line dropped a real blind
            # lane on the way out.
            if scoring_must_stop(v, ws):
                stopped = v
                break

        # The seal relocates the held-out set in BOTH modes — the campaign must
        # lose file access early — but selfcheck neither scores it nor reports
        # it. Revealing `holdout_detected` to whoever runs the check is enough to
        # steer hardening: seeing 3/5 says "keep tuning" even with the files out
        # of reach. The held-out result belongs to the final gate alone.
        held = []
        if mode != "selfcheck" and stopped is None:
            for i, m in enumerate(held_meta):
                v = score_mutant(m, config, corpus, canon, refs, ws, 1000 + i)
                held.append(v)
                if scoring_must_stop(v, ws):
                    stopped = v
                    break
        if stopped is not None:
            note(report, "scoring stopped after mutant %s: %s. The tree or the app is no longer "
                 "KNOWN to be at baseline, so the mutants after it were not scored and are absent "
                 "from the counts; a leftover, if any, is on record for the next gate."
                 % (stopped.get("id"), stopped.get("reason") or "its revert did not restore the baseline"))

        valid = [v for v in verdicts if v.get("valid")]
        detected = [v for v in valid if v.get("detected")]
        # LES INVALIDES, EN CLAIR ET STRUCTURES. Ils etaient deja dits, en texte
        # libre au milieu du log. Un mutant devient invalide pour deux raisons
        # tres differentes — il ne mute rien (il n'a jamais rien prouve), ou son
        # point d'appui a disparu sous un changement legitime (il prouvait
        # quelque chose et ne le prouve plus). Le second cas est mecanique et
        # reparable ; le distinguer demande de pouvoir le LIRE, pas de le
        # deviner dans une phrase.
        report["invalid"] = [{"id": v["id"], "reason": v.get("reason", "")}
                             for v in verdicts if not v.get("valid")]
        report.update(
            total=len(verdicts),
            valid=len(valid),
            detected=len(detected),
            score_pct=int(100 * len(detected) / len(valid)) if valid else 0,
            revert_clean=overall_revert_clean(verdicts + held, stopped, ws),
            collateral=sum(len(v.get("collateral") or []) for v in verdicts),
        unstable_controls=sorted({c for v in verdicts
                                  for c in (v.get("unstable_controls") or [])}),
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
            note(report, (
                "%d held-out mutant(s) were detected by a lane OTHER than the one they "
                "probe: %s. The net saw the change; the lane they were drawn against did "
                "not. Citing the aggregate alone would report the resemblance."
                % (len(off), ", ".join(off))))
        # A lot whose baseline was lost reports a WITHHELD held-out figure, not
        # a measured one: the held-out loop did not run (or stopped part-way),
        # so its 0 == 0 is the vacuous equality this file guards against
        # everywhere else — and it sat right beside a `revert_clean` that read
        # silence as clean. Measured, end to end: apply.sh half-edits and exits
        # 7, revert.sh exits 1 — score_pct 100, revert_clean true, blind_lanes
        # empty, holdout 0/0, GATE GREEN, `git status` showing `M f.txt` and the
        # marker still armed. The gate approved the exact tree this guard exists
        # to refuse. `overall_revert_clean` is now the one that says otherwise.
        baseline_lost = not report["revert_clean"]
        if baseline_lost and len(held) < len(held_meta):
            # Withheld, not zero-because-failed — the idiom selfcheck uses just
            # below, deliberately unequal so an unscored held-out set can never
            # be taken for a passed one.
            report["holdout_total"] = len(held_meta)
            report["holdout_detected"] = -1
        if mode == "selfcheck":
            # Withheld, not zero-because-failed. The two numbers are made
            # DELIBERATELY UNEQUAL: the gate converges on
            # `holdout_detected == holdout_total`, so a selfcheck report that
            # ever reached the gate must fail it rather than sail through on a
            # -1 == -1 coincidence.
            report["holdout_total"] = len(held_meta)
            report["holdout_detected"] = -1

        problems = []
        if baseline_lost:
            problems.append(
                "THE TREE IS NOT KNOWN TO BE BACK AT BASELINE: %s. The figures above "
                "describe what was measured BEFORE that point, not the lot — the mutants "
                "after it were never applied, so an empty blind-lane list and a 0/0 "
                "held-out figure mean 'not measured', never 'clean'."
                % (("scoring stopped after mutant %s" % stopped.get("id")) if stopped
                   else "a mutant is still recorded as applied"))
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

        if report["unstable_controls"]:
            problems.append(
                "%d control entry(ies) DO NOT REPRODUCE THEMSELVES: %s. They differed "
                "from their reference while a mutant was applied AND still differed once "
                "it was reverted, so the mutant is not the cause — the entry is. Every "
                "verdict about such an entry is meaningless, including the greens: it is "
                "compared against a reference it cannot reproduce. `stable` does not cover "
                "this (it compares three captures to each other, taken within one run) and "
                "neither does the negative control (it catches permanent drift, not rare "
                "drift). Canonicalise what moves, or make the fixture pin it."
                % (len(report["unstable_controls"]), ", ".join(report["unstable_controls"])))
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
            note(report, "MODE=selfcheck — the held-out set was sealed but NOT scored; "
                         "its result is withheld on purpose. Only the final gate scores "
                         "it. An empty log_tail here means the VISIBLE set is clean, "
                         "which is not the same as a green gate.")
        elif (not baseline_lost and report["holdout_total"]
                and report["holdout_detected"] < report["holdout_total"]):
            # `not baseline_lost`: after a stop the figure is WITHHELD (-1), not
            # measured, and rendering it would read "HELD-OUT set: -1/2 detected"
            # — a count where there was no measurement. The red is already said,
            # once, above.
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
            note(report, (
                "git could not be asked whether %s are ignored — this workspace is not a "
                "checkout (a copy, or no git available). They are PRESENT, which is what a "
                "copy of tracked files proves; that they are TRACKED is verified only where "
                "a checkout exists." % ", ".join(unanswerable)))
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
