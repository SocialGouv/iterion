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

const [port, url, axePath, cookiePath] = process.argv.slice(2);
if (!port || !url || !axePath) {
  console.error("usage: run-axe.mjs <port-cdp> <url> <axe.min.js> [cookies.json]");
  process.exit(2);
}

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
ws.addEventListener("message", (e) => {
  const m = JSON.parse(e.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  else if (m.method) events.push(m.method);
});
await new Promise((res, rej) => {
  ws.addEventListener("open", res);
  ws.addEventListener("error", () => rej(new Error("socket CDP inutilisable")));
}).catch((e) => fail(String(e)));

async function cmd(method, params = {}) {
  const r = await new Promise((res) => {
    pending.set(++seq, res);
    ws.send(JSON.stringify({ id: seq, method, params }));
  });
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

const timeout = new Promise((_, rej) => setTimeout(() => rej(new Error("chargement > 60 s")), 60000));
await Promise.race([loaded, timeout]).catch((e) => fail(String(e.message || e)));

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

// Le réseau se tait avant l'audit : les pages remplies côté client ne portent
// leur contenu qu'après leurs appels d'API, et auditer avant ne mesurerait que
// le squelette. Fenêtre de silence courte, plafond dur.
await cmd("Runtime.evaluate", {
  expression: `new Promise((res) => {
    let last = performance.now();
    const seen = new PerformanceObserver(() => { last = performance.now(); });
    seen.observe({ entryTypes: ["resource"] });
    const deadline = performance.now() + 10000;
    (function poll() {
      if (performance.now() - last > 800 || performance.now() > deadline) { seen.disconnect(); res(true); }
      else setTimeout(poll, 100);
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
