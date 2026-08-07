// Audit d'accessibilité d'UNE page, rendue par un vrai navigateur.
//
// La plomberie DevTools — onglet neuf, cookies du persona, navigation, attente
// que la page soit prête, fermeture — vit dans `../browser/cdp.mjs`, partagée
// avec les autres lanes qui rendent une page. Ce fichier ne garde que ce qui
// est propre à l'audit.
//
// Usage :  node run-axe.mjs <port-cdp> <url> <axe.min.js> [cookies.json] [timeout-s]
// Sortie :  le résultat axe brut, sur stdout. La mise en forme canonique est
//           faite ailleurs, par `canon/rules.py` : ce script transporte, il ne
//           juge pas.
import { readFileSync } from "node:fs";
import { openPage, emit } from "../browser/cdp.mjs";

const [port, url, axePath, cookiePath, loadTimeoutArg] = process.argv.slice(2);
if (!port || !url || !axePath) {
  console.error("usage: run-axe.mjs <port-cdp> <url> <axe.min.js> [cookies.json] [timeout-s]");
  process.exit(2);
}

// Le plafond d'ANALYSE est distinct de celui du protocole : `axe.run` sur un
// tableau dense parcourt tout l'arbre et calcule un contraste par nœud. Mesure :
// une page dense — une liste de vingt lignes, 74 nœuds fautifs — a
// dépassé les 60 s et le job est tombé sur un plafond de PROTOCOLE alors que
// rien n'était bloqué. Le chargement de page, lui, disposait déjà de 240 s
// configurables : c'est la même nature de dépense, elle n'avait simplement pas
// le même budget.
const ANALYSIS_TIMEOUT_MS = (Number(process.env.GM_A11Y_ANALYSIS_TIMEOUT_S) || 300) * 1000;

// Plafond de chargement, passé par l'appelant. Il était figé à 60 s : un pari
// sur la machine la plus rapide qui exécuterait ce filet, perdu au premier
// passage de CI à froid — application qui vient de démarrer, JVM sans code
// compilé, navigateur qui s'initialise. Un plafond de chargement est un filet
// de sécurité contre un blocage, pas une assertion de performance ; le régler
// serré ne mesure rien de plus et rend rouge pour la vitesse de l'hôte.
const page = await openPage({
  port, url, cookiePath, who: "run-axe",
  loadTimeoutS: Number(loadTimeoutArg) || 240,
});

await page.cmd("Runtime.evaluate", { expression: readFileSync(axePath, "utf8") }, ANALYSIS_TIMEOUT_MS);

const ran = await page.cmd("Runtime.evaluate", {
  expression: "axe.run(document, {resultTypes: ['violations'], reporter: 'v1'})"
    + "  .then(r => JSON.stringify({violations: r.violations, testEngine: r.testEngine}))",
  awaitPromise: true,
  returnByValue: true,
}, ANALYSIS_TIMEOUT_MS);

const value = ran?.result?.value;
// Un audit qui ne rend rien doit CRIER. Sorti vide, il se canonicaliserait en
// « aucune anomalie » : le meilleur résultat possible, et une lane aveugle.
if (typeof value !== "string" || !value.length) {
  page.fail("axe n'a rien rendu sur " + url + " — résultat brut : " + JSON.stringify(ran).slice(0, 400));
}
await page.closeTarget();
page.ws.close();
emit(value);
