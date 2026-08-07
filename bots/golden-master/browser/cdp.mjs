// Plomberie DevTools partagée par les lanes qui rendent une page dans un vrai
// navigateur : ouvrir un onglet neuf, y poser les cookies du persona, naviguer,
// attendre que la page soit VRAIMENT prête, et refermer.
//
// Ce module existe parce que la connaissance qu'il porte a été payée cher, et
// qu'une deuxième copie l'aurait perdue. Quatre endroits de ce dépôt ont
// recalculé le même chemin d'état ; les quatre ont dérivé. Une lane qui
// recopierait ces cent lignes dériverait pareil, et le jour où elle dériverait,
// elle ne tomberait pas : elle rendrait un résultat stable, plausible et faux.
//
// Objets intégrés de node uniquement — `fetch` et `WebSocket` sont globaux
// depuis node 21, et le protocole DevTools se parle en JSON sur une socket.
// Aucune dépendance npm : le filet doit tourner dans un bac à sable sans sortie
// réseau, et une lane qui exigerait `npm install` cesserait d'être rejouable là
// où on en a le plus besoin.
//
// Pas de gabarit `${...}` dans ce fichier, ni dans ceux qui l'importent : la
// même source est portée par un DSL qui développe les expressions
// dollar-accolade AVANT d'exécuter le script, et une seule suffirait à rendre le
// fichier illisible — avec une erreur qui n'apparaîtrait que chez la cible.
import { readFileSync } from "node:fs";

export const CMD_TIMEOUT_MS = 60000;

/** Ouvre une page prête à être mesurée. Rend `{cmd, close, fail}`.
 *
 * `who` nomme l'appelant dans les messages d'erreur : un diagnostic qui ne dit
 * pas QUI a échoué envoie chercher au mauvais endroit.
 */
export async function openPage(opts) {
  const { port, url, cookiePath, who } = opts;
  const loadTimeoutMs = (Number(opts.loadTimeoutS) || 240) * 1000;
  const quietMs = Number(opts.quietMs) || 1200;
  const quietDeadlineMs = Number(opts.quietDeadlineMs) || 15000;

  const fail = (msg) => { console.error(who + ": " + msg); process.exit(1); };

  // Une cible neuve par page. Réutiliser un onglet garderait l'état de la
  // précédente — styles injectés, scripts encore vivants, focus — et la
  // deuxième page serait mesurée dans le résidu de la première.
  const created = await fetch("http://127.0.0.1:" + port + "/json/new?about:blank", { method: "PUT" })
    .then((r) => r.json())
    .catch((e) => fail("Chromium ne répond pas sur le port " + port + " (" + e + ")"));
  if (!created.webSocketDebuggerUrl) fail("cible créée sans URL de débogage : " + JSON.stringify(created));

  // L'ONGLET EST FERMÉ, toujours, et pas seulement la socket.
  //
  // `/json/new` crée une CIBLE ; `ws.close()` ne ferme que le canal de contrôle.
  // L'onglet, lui, survivait au script avec sa page rendue et le moteur injecté
  // dedans. Un audit par entrée, trois captures par passage de porte : 18
  // onglets à six écrans — supportable et donc invisible — mais **60 à vingt
  // écrans**, dont des tableaux de plusieurs dizaines de lignes.
  //
  // Mesure du 2026-08-04 : le navigateur affamait la machine, et c'est
  // l'APPLICATION qui tombait. Les symptômes ne se ressemblaient pas d'un
  // passage à l'autre — `ERR_CONNECTION_REFUSED` sur la première entrée ici,
  // `Runtime.evaluate` sans réponse là — parce qu'une pénurie de ressources ne
  // tombe pas deux fois au même endroit. Aucun ne désignait le navigateur.
  //
  // La fermeture passe par HTTP et non par CDP : elle doit rester possible quand
  // la socket est déjà morte, ce qui est précisément le cas où l'onglet fuit.
  const closeTarget = async () => {
    if (!created.id) return;
    try {
      await fetch("http://127.0.0.1:" + port + "/json/close/" + created.id);
    } catch (e) { /* le navigateur est deja parti : rien a liberer */ }
  };
  process.on("exit", () => {
    if (created.id) { try { fetch("http://127.0.0.1:" + port + "/json/close/" + created.id); } catch (e) {} }
  });

  const ws = new WebSocket(created.webSocketDebuggerUrl);
  let seq = 0;
  const pending = new Map();
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
  //
  // Le plafond est PAR APPEL, parce que tous les appels ne coûtent pas la même
  // chose. Soixante secondes suffisent à un aller-retour de protocole ; elles ne
  // suffisent pas à une analyse qui parcourt tout l'arbre. Mesure : l'audit de
  // l'entrée 058 a dépassé les 60 s et le job est tombé sur un plafond de
  // PROTOCOLE alors que rien n'était bloqué.
  async function cmd(method, params = {}, timeoutMs = CMD_TIMEOUT_MS) {
    const id = ++seq;
    const r = await Promise.race([
      new Promise((res) => { pending.set(id, res); ws.send(JSON.stringify({ id, method, params })); }),
      new Promise((res) => setTimeout(() => res({ __timeout: true }), timeoutMs)),
    ]);
    if (r.__timeout) {
      pending.delete(id);
      fail(method + " est reste sans reponse pendant " + (timeoutMs / 1000)
           + " s" + (crashed ? " — " + crashed : " et le navigateur n'a rien signale"));
    }
    if (r.error) fail(method + " a échoué : " + JSON.stringify(r.error));
    // Une exception DANS LA PAGE n'est pas une erreur de protocole : le
    // protocole a parfaitement fonctionné, et il rend `exceptionDetails` à côté
    // d'un résultat vide. Sans ce contrôle, une sonde qui lève devient un
    // `undefined` silencieux chez l'appelant — qui l'écrit alors dans une
    // référence, ou pire, la canonicalise en « rien à signaler ».
    //
    // Mesuré sur ce fichier même : un diagnostic dont l'expression levait a
    // rendu la chaîne « undefined », sans un mot, et l'accusation est partie
    // sur le navigateur.
    const ex = r.result && r.result.exceptionDetails;
    if (ex) {
      const d = ex.exception || {};
      fail(method + " a levé dans la page : "
           + (d.description || d.value || ex.text || JSON.stringify(ex)).toString().slice(0, 600));
    }
    return r.result;
  }

  await cmd("Page.enable");
  await cmd("Runtime.enable");

  // La fenêtre est DÉCLARÉE par l'appelant, ou pas du tout — jamais devinée à
  // moitié. Le navigateur est lancé avec `--force-device-scale-factor=1` mais
  // sans `--window-size` : sa taille est donc celle du défaut du mode sans
  // affichage. Pour un audit d'arbre, hériter de ce défaut est sans
  // conséquence ; pour une lane qui mesure des PIXELS, c'est bâtir la référence
  // sur une valeur que personne n'a choisie et qu'une montée du navigateur peut
  // déplacer. Un canevas se dimensionne sur la mise en page.
  //
  // L'absence de valeur par défaut ici est délibérée : en poser une déplacerait
  // les références des lanes déjà enregistrées, pour une commodité.
  if (opts.viewport) {
    await cmd("Emulation.setDeviceMetricsOverride", {
      width: opts.viewport.width, height: opts.viewport.height,
      deviceScaleFactor: 1, mobile: false,
    });
  }

  // Le navigateur est PARTAGE entre les captures : les cookies sont donc effaces
  // AVANT de poser ceux du persona, et sans condition.
  //
  // `Network.setCookie` ne fait qu'AJOUTER. Pour un persona anonyme la liste est
  // vide, si bien que l'ancienne version ne touchait a rien — pas meme
  // `Network.enable` — et la session du persona precedent restait en place. Une
  // page publique mesuree apres une page d'administration etait alors rendue
  // CONNECTEE : en-tete different, liens `logout` et `mon-profil` en plus, et
  // deux violations de contraste supplementaires.
  //
  // Le defaut dependait de l'ORDRE des captures, donc invisible tant que
  // l'echantillon de controle ne tirait pas deux entrees de personas differents
  // coup sur coup. Mesure : une entree anonyme
  // capturee juste apres une entree d'administration — `NODES 8` attendus, `NODES 10`
  // observes, et le mutant sans rapport qui tournait a ce moment-la s'est vu
  // imputer le collateral.
  //
  // Le cas anonyme est celui qui en a le plus besoin : c'est le seul ou rien ne
  // vient ecraser ce qui traine.
  await cmd("Network.enable");
  await cmd("Network.clearBrowserCookies");
  if (cookiePath) {
    const cookies = JSON.parse(readFileSync(cookiePath, "utf8"));
    for (const c of cookies) await cmd("Network.setCookie", c);
  }

  // `Page.loadEventFired` plutôt qu'un délai fixe. Un délai est un pari sur la
  // machine la plus lente qui exécutera un jour ce filet ; perdu, il produit une
  // mesure d'une page à moitié construite — stable, plausible, et fausse.
  const loaded = new Promise((res) => {
    const onMsg = (e) => {
      const m = JSON.parse(e.data);
      if (m.method === "Page.loadEventFired") { ws.removeEventListener("message", onMsg); res(); }
    };
    ws.addEventListener("message", onMsg);
  });
  // `Page.navigate` rend `errorText` quand la navigation échoue, et le navigateur
  // affiche alors SA propre page d'erreur — qui se charge, se rend, et se mesure
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
  const timeout = new Promise((res) => setTimeout(() => res("TIMEOUT"), loadTimeoutMs));
  const outcome = await Promise.race([loaded.then(() => "LOADED"), timeout]);
  if (outcome === "TIMEOUT" && crashed) {
    fail("le navigateur n'a jamais fini de charger " + url + " — " + crashed);
  }
  if (outcome === "TIMEOUT") {
    let diag = "(diagnostic indisponible)";
    try {
      const d = await cmd("Runtime.evaluate", {
        expression: "JSON.stringify({"
          + "ready: document.readyState,"
          + "href: location.href,"
          + "pending: performance.getEntriesByType('resource')"
          + "            .filter(function (r) { return r.responseEnd === 0; })"
          + "            .map(function (r) { return r.name; }).slice(0, 12),"
          + "total: performance.getEntriesByType('resource').length"
          + "})",
        returnByValue: true,
      });
      diag = d?.result?.value || diag;
    } catch (e) { diag = "(diagnostic impossible : " + e + ")"; }
    fail("l'evenement `load` n'est pas venu en " + (loadTimeoutMs / 1000)
         + " s sur " + url + " — etat de la page : " + diag);
  }

  // Deuxième verrou, indépendant du premier : l'origine réellement chargée. Une
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

  // Le réseau se tait ET le DOM cesse de bouger avant la mesure.
  //
  // Le réseau seul ne suffit pas : une page remplie côté client construit encore
  // son arbre après sa dernière réponse — graphiques, listes, composants montés
  // sur les données reçues. Mesurer à ce moment saisit un état transitoire, et le
  // résultat dépend alors de la vitesse de la machine.
  //
  // Mesuré : l'audit d'une page remplie côté client était stable en local sur trois captures
  // et DIVERGEAIT en intégration continue sous une mutation NULLE. Un filet qui
  // bouge sans que rien n'ait bougé ne prouve rien quand il bouge pour une vraie
  // raison — c'est le juge hystérique, l'exacte symétrie du juge aveugle.
  //
  // Deux observateurs, un seul verdict : la page est prête quand ni requête ni
  // mutation n'est survenue depuis la fenêtre de silence. Plafond dur pour qu'une
  // page à animation perpétuelle n'immobilise pas le filet.
  //
  // BORNE CONNUE, et c'est la raison d'être de la lane `canvas` : un canevas qui
  // se peint ne produit NI requête NI mutation du DOM. Ces deux observateurs ne
  // le voient pas, et ne peuvent pas le voir. Qui mesure un canevas doit attendre
  // son raster, pas son arbre.
  await cmd("Runtime.evaluate", {
    expression: "new Promise((res) => {"
      + "  let last = performance.now();"
      + "  const touch = () => { last = performance.now(); };"
      + "  const net = new PerformanceObserver(touch);"
      + "  net.observe({ entryTypes: ['resource'] });"
      + "  const dom = new MutationObserver(touch);"
      + "  dom.observe(document.documentElement,"
      + "              {childList: true, subtree: true, attributes: true, characterData: true});"
      + "  const deadline = performance.now() + " + quietDeadlineMs + ";"
      + "  (function poll() {"
      + "    if (performance.now() - last > " + quietMs + " || performance.now() > deadline) {"
      + "      net.disconnect(); dom.disconnect(); res(true);"
      + "    } else setTimeout(poll, 100);"
      + "  })();"
      + "})",
    awaitPromise: true,
  });

  return { cmd, fail, closeTarget, ws };
}

/** Écrit le résultat sur stdout et sort proprement.
 *
 * Le rappel d'écriture, PAS `process.exit` juste après. Sur un tube, une
 * écriture est asynchrone : sortir immédiatement coupe à la frontière du tampon
 * — 65 450 octets sur 71 000, au milieu d'une chaîne. Le canonicaliseur a refusé
 * le JSON tronqué, ce qui est le comportement voulu, mais l'accusation portait
 * sur le mauvais composant.
 */
export function emit(value) {
  process.stdout.write(value, () => process.exit(0));
}
