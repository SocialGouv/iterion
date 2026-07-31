// Audit d'accessibilité d'UNE page, rendue par un vrai navigateur.
//
// Objets intégrés de node uniquement — `fetch` et `WebSocket` sont globaux
// depuis node 21, et le protocole DevTools se parle en JSON sur une socket.
// Aucune dépendance npm : le reste du filet tient en bibliothèque standard pour
// pouvoir tourner dans un bac à sable sans sortie réseau, et une lane qui
// exigerait `npm install` cesserait d'être rejouable là où on en a le plus
// besoin.
//
// Usage :  node run-axe.mjs <port-cdp> <url> <axe.min.js> [cookies.json]
// Sortie :  le résultat axe brut, sur stdout. La mise en forme canonique est
//           faite ailleurs, par `canon/rules.py` : ce script transporte, il ne
//           juge pas.
import { readFileSync } from "node:fs";

const [port, url, axePath, cookiePath, loadTimeoutArg] = process.argv.slice(2);
if (!port || !url || !axePath) {
  console.error("usage: run-axe.mjs <port-cdp> <url> <axe.min.js> [cookies.json] [timeout-s]");
  process.exit(2);
}
// Plafond de chargement, passé par l'appelant. Il était figé à 60 s : un pari
// sur la machine la plus rapide qui exécuterait ce filet, perdu au premier
// passage de CI à froid — application qui vient de démarrer, JVM sans code
// compilé, navigateur qui s'initialise. Un plafond de chargement est un filet
// de sécurité contre un blocage, pas une assertion de performance ; le régler
// serré ne mesure rien de plus et rend rouge pour la vitesse de l'hôte.
const LOAD_TIMEOUT_MS = (Number(loadTimeoutArg) || 240) * 1000;

const fail = (msg) => { console.error("run-axe: " + msg); process.exit(1); };

// Une cible neuve par page. Réutiliser un onglet garderait l'état de la
// précédente — styles injectés, scripts encore vivants, focus — et la
// deuxième page serait auditée dans le résidu de la première.
// Concaténation, pas de littéral gabarit. Ce fichier est aussi émis depuis le
// DSL du bot, qui développe les expressions dollar-accolade AVANT d'exécuter le
// script : une seule interpolation JavaScript de cette forme suffirait à rendre
// le fichier illisible, et l'erreur n'apparaîtrait que chez la cible. Ce
// commentaire lui-même en est dépourvu, pour la même raison.
const created = await fetch("http://127.0.0.1:" + port + "/json/new?about:blank", { method: "PUT" })
  .then((r) => r.json())
  .catch((e) => fail("Chromium ne répond pas sur le port " + port + " (" + e + ")"));
if (!created.webSocketDebuggerUrl) fail("cible créée sans URL de débogage : " + JSON.stringify(created));

const ws = new WebSocket(created.webSocketDebuggerUrl);
let seq = 0;
const pending = new Map();
const events = [];
// Un onglet dont le processus de rendu MEURT ne répond plus à rien : ni
// `Page.loadEventFired`, ni la moindre évaluation. Le script attendait alors
// pour toujours, node signalait « unsettled top-level await », et le seul
// message visible parlait d'un chargement trop long. Trois hypothèses ont été
// dépensées à chercher une lenteur là où il y avait un mort.
let crashed = null;
ws.addEventListener("message", (e) => {
  const m = JSON.parse(e.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  else if (m.method) {
    events.push(m.method);
    if (m.method === "Inspector.targetCrashed") crashed = "le processus de rendu a plante";
    if (m.method === "Inspector.detached") crashed = "session CDP detachee : " + JSON.stringify(m.params || {});
  }
});
ws.addEventListener("close", () => { if (!crashed) crashed = "socket CDP fermee par le navigateur"; });
await new Promise((res, rej) => {
  ws.addEventListener("open", res);
  ws.addEventListener("error", () => rej(new Error("socket CDP inutilisable")));
}).catch((e) => fail(String(e)));

// AUCUNE commande n'attend indéfiniment. Sans ce plafond, une commande sans
// réponse suspend le script entier, et l'échec se présente comme un `await` non
// résolu — un symptôme qui ne désigne ni la commande, ni la cause.
const CMD_TIMEOUT_MS = 60000;
async function cmd(method, params = {}) {
  const id = ++seq;
  const r = await Promise.race([
    new Promise((res) => { pending.set(id, res); ws.send(JSON.stringify({ id, method, params })); }),
    new Promise((res) => setTimeout(() => res({ __timeout: true }), CMD_TIMEOUT_MS)),
  ]);
  if (r.__timeout) {
    pending.delete(id);
    fail(method + " est reste sans reponse pendant " + (CMD_TIMEOUT_MS / 1000)
         + " s" + (crashed ? " — " + crashed : " et le navigateur n'a rien signale"));
  }
  if (r.error) fail(method + " a échoué : " + JSON.stringify(r.error));
  return r.result;
}

await cmd("Page.enable");
await cmd("Runtime.enable");

if (cookiePath) {
  const cookies = JSON.parse(readFileSync(cookiePath, "utf8"));
  if (cookies.length) await cmd("Network.enable");
  for (const c of cookies) await cmd("Network.setCookie", c);
}

// `Page.loadEventFired` plutôt qu'un délai fixe. Un délai est un pari sur la
// machine la plus lente qui exécutera un jour ce filet ; perdu, il produit un
// audit d'une page à moitié construite — stable, plausible, et faux.
const loaded = new Promise((res) => {
  const onMsg = (e) => {
    const m = JSON.parse(e.data);
    if (m.method === "Page.loadEventFired") { ws.removeEventListener("message", onMsg); res(); }
  };
  ws.addEventListener("message", onMsg);
});
// `Page.navigate` rend `errorText` quand la navigation échoue, et le navigateur
// affiche alors SA propre page d'erreur — qui se charge, se rend, et s'audite
// très bien. Sans ce contrôle, un port mort produit un résultat parfaitement
// formé, stable, identique pour toutes les pages, et faux. Vu : six entrées,
// six fois le même verdict, parce que l'URL publiée venait d'une autre copie de
// travail.
const nav = await cmd("Page.navigate", { url });
if (nav.errorText) fail("navigation vers " + url + " refusée : " + nav.errorText);

// Le plafond ne se contente pas d'échouer : il DIT ce qui pendait. Un message
// qui rapporte « chargement > N s » accuse la lenteur de l'hôte et envoie
// chercher au mauvais endroit — deux hypothèses ont été dépensées ainsi. Ce que
// le lecteur a besoin de savoir tient en deux faits que le navigateur connaît :
// où en est le document, et quelles requêtes ne sont pas terminées.
const timeout = new Promise((res) => setTimeout(() => res("TIMEOUT"), LOAD_TIMEOUT_MS));
const outcome = await Promise.race([loaded.then(() => "LOADED"), timeout]);
if (outcome === "TIMEOUT" && crashed) {
  fail("le navigateur n'a jamais fini de charger " + url + " — " + crashed);
}
if (outcome === "TIMEOUT") {
  let diag = "(diagnostic indisponible)";
  try {
    const d = await cmd("Runtime.evaluate", {
      expression: `JSON.stringify({
        ready: document.readyState,
        href: location.href,
        pending: performance.getEntriesByType("resource")
                    .filter(function (r) { return r.responseEnd === 0; })
                    .map(function (r) { return r.name; }).slice(0, 12),
        total: performance.getEntriesByType("resource").length
      })`,
      returnByValue: true,
    });
    diag = d?.result?.value || diag;
  } catch (e) { diag = "(diagnostic impossible : " + e + ")"; }
  fail("l'evenement `load` n'est pas venu en " + (LOAD_TIMEOUT_MS / 1000)
       + " s sur " + url + " — etat de la page : " + diag);
}

// Deuxième vérrou, indépendant du premier : l'origine réellement chargée. Une
// redirection vers la page de connexion ne serait pas une erreur de navigation
// et passerait le contrôle ci-dessus ; un schéma `chrome-error:` non plus, si
// jamais l'erreur n'était pas remontée.
const where = await cmd("Runtime.evaluate", {
  expression: "JSON.stringify({href: location.href, ready: document.readyState})",
  returnByValue: true,
});
const here = JSON.parse(where?.result?.value || "{}");
if (!here.href || !here.href.startsWith(new URL(url).origin)) {
  fail("la page chargée est " + (here.href || "inconnue") + ", pas " + url
       + " — le navigateur n'a pas atteint l'application");
}

// Le réseau se tait ET le DOM cesse de bouger avant l'audit.
//
// Le réseau seul ne suffit pas : une page remplie côté client construit encore
// son arbre après sa dernière réponse — graphiques, listes, composants montés
// sur les données reçues. Auditer à ce moment mesure un état transitoire, et le
// résultat dépend alors de la vitesse de la machine.
//
// Mesuré : l'audit du tableau de bord était stable en local sur trois captures
// et DIVERGEAIT en intégration continue sous une mutation NULLE. Un filet qui
// bouge sans que rien n'ait bougé ne prouve rien quand il bouge pour une vraie
// raison — c'est le juge hystérique, l'exacte symétrie du juge aveugle.
//
// Deux observateurs, un seul verdict : la page est prête quand ni requête ni
// mutation n'est survenue depuis la fenêtre de silence. Plafond dur pour qu'une
// page à animation perpétuelle n'immobilise pas le filet.
await cmd("Runtime.evaluate", {
  expression: `new Promise((res) => {
    let last = performance.now();
    const touch = () => { last = performance.now(); };
    const net = new PerformanceObserver(touch);
    net.observe({ entryTypes: ["resource"] });
    const dom = new MutationObserver(touch);
    dom.observe(document.documentElement,
                {childList: true, subtree: true, attributes: true, characterData: true});
    const deadline = performance.now() + 15000;
    (function poll() {
      if (performance.now() - last > 1200 || performance.now() > deadline) {
        net.disconnect(); dom.disconnect(); res(true);
      } else setTimeout(poll, 100);
    })();
  })`,
  awaitPromise: true,
});

await cmd("Runtime.evaluate", { expression: readFileSync(axePath, "utf8") });

const ran = await cmd("Runtime.evaluate", {
  expression: `axe.run(document, {resultTypes: ["violations"], reporter: "v1"})
                  .then(r => JSON.stringify({violations: r.violations, testEngine: r.testEngine}))`,
  awaitPromise: true,
  returnByValue: true,
});

const value = ran?.result?.value;
// Un audit qui ne rend rien doit CRIER. Sorti vide, il se canonicaliserait en
// « aucune anomalie » : le meilleur résultat possible, et une lane aveugle.
if (typeof value !== "string" || !value.length) {
  fail("axe n'a rien rendu sur " + url + " — résultat brut : " + JSON.stringify(ran).slice(0, 400));
}
ws.close();
// Le rappel d'écriture, PAS `process.exit` juste après. Sur un tube, une
// écriture est asynchrone : sortir immédiatement coupe à la frontière du tampon
// — 65 450 octets sur 71 000, au milieu d'une chaîne. Le canonicaliseur a refusé
// le JSON tronqué, ce qui est le comportement voulu, mais l'accusation portait
// sur le mauvais composant.
process.stdout.write(value, () => process.exit(0));
