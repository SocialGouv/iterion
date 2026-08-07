// Ce qu'un CANEVAS a réellement peint — la surface qu'aucune autre lane ne voit.
//
// POURQUOI CETTE LANE EXISTE, et le défaut précis qu'elle interdit.
//
// Les autres lanes observent des octets servis ou un arbre de document. Un
// graphique dessiné sur un canevas n'est ni l'un ni l'autre : le HTML servi
// contient une balise vide, et le DOM ne bouge plus une fois le canevas peint.
// Un audit d'accessibilité le dit lui-même — « une donnée rendue
// uniquement en couleur ou en canevas n'est pas restituée ». Conséquence
// mesurée : un graphique qui cesserait complètement de se dessiner laisserait
// TOUTES les références de ce dépôt identiques à l'octet près.
//
// C'est la forme exacte du juge aveugle : un contrôle qui établit quelque chose
// de proche de ce qu'il annonce, et rapporte la ressemblance. Un PDF
// structurellement valide sans un seul caractère passait au vert chez d'autres ;
// ici ce serait un canevas structurellement présent sans un seul pixel.
//
// TROIS ASSERTIONS INDÉPENDANTES, et il en faut trois :
//
//   1. `INKED` — le nombre de pixels réellement peints. Zéro est le cas aveugle,
//      et lui seul le tue. Aucune lecture de configuration ne peut le remplacer :
//      une bibliothèque de graphiques garde ses données en mémoire même quand
//      son rendu échoue.
//   2. `CHART` — ce que la bibliothèque déclare dessiner : type, étiquettes,
//      valeurs, couleurs. C'est la couche SÉMANTIQUE, celle qui dit qu'un
//      camembert n'est pas devenu un histogramme et que les données sont les
//      mêmes. Stable, lisible, et diffable ligne à ligne.
//   3. `RASTER` — une signature grossière de l'image, insensible au lissage des
//      bords mais sensible à une forme ou une couleur qui change. Elle attrape
//      ce que les deux autres laissent passer : un rendu qui dessine autre chose
//      que ce qu'il déclare.
//
// Chacune peut être vraie pendant que les autres sont fausses. C'est pour ça
// qu'elles sont trois, et qu'aucune n'est agrégée avec les autres.
//
// Usage :  node run-canvas.mjs <port-cdp> <url> [cookies.json] [timeout-s]
// Sortie :  JSON brut sur stdout. La mise en forme canonique est faite par
//           `canon/rules.py` : ce script transporte, il ne juge pas.
import { openPage, emit } from "../browser/cdp.mjs";

const [port, url, cookiePath, loadTimeoutArg] = process.argv.slice(2);
if (!port || !url) {
  console.error("usage: run-canvas.mjs <port-cdp> <url> [cookies.json] [timeout-s]");
  process.exit(2);
}

// La fenêtre est déclarée ici, pas héritée du défaut du navigateur : un canevas
// se dimensionne sur la mise en page, et une référence de pixels bâtie sur une
// taille que personne n'a choisie se déplace le jour où le navigateur monte.
const VIEWPORT = { width: 1280, height: 900 };

// Grille de la signature : 16×16 cellules, chaque canal quantifié sur 4 bits.
// Le lissage des bords d'un arc fait varier quelques pixels ; moyennés sur une
// cellule de plusieurs centaines puis quantifiés sur seize niveaux, ils ne
// déplacent pas la signature. Une forme ou une couleur qui change, si.
// Ces deux nombres sont dans la référence : ils font partie de ce qui est
// mesuré, pas de la façon dont on l'a mesuré.
const GRID = 16;
const QUANT_BITS = 4;

const page = await openPage({
  port, url, cookiePath, who: "run-canvas",
  loadTimeoutS: Number(loadTimeoutArg) || 240,
  viewport: VIEWPORT,
});

// LE RASTER SE STABILISE, et c'est une attente distincte de celle du DOM.
//
// `openPage` attend que le réseau se taise et que l'arbre cesse de bouger. Une
// bibliothèque de graphiques anime son entrée pendant environ une seconde en
// repeignant le canevas : ni requête, ni mutation. Les deux observateurs de
// `openPage` sont donc déjà satisfaits pendant que l'image change encore, et
// capturer là donnerait une référence prise au milieu d'une animation —
// différente à chaque passage, et le filet accuserait le produit.
//
// On attend donc que DEUX relevés consécutifs du raster soient identiques. Le
// relevé est un condensé bon marché (une somme par canal sur un échantillon
// régulier), pas l'image entière : il s'agit de savoir si ça bouge, pas quoi.
const SETTLE_POLL_MS = 150;
const SETTLE_DEADLINE_MS = 20000;
const settled = await page.cmd("Runtime.evaluate", {
  expression: "(async function () {"
    + "  const cvs = () => Array.prototype.slice.call(document.querySelectorAll('canvas'));"
    + "  const probe = () => cvs().map(function (c) {"
    + "    const ctx = c.getContext('2d');"
    + "    if (!ctx || !c.width || !c.height) return c.width + 'x' + c.height + ':-';"
    + "    const d = ctx.getImageData(0, 0, c.width, c.height).data;"
    + "    let a = 0, b = 0, n = 0;"
    + "    for (let i = 0; i < d.length; i += 4 * 97) { a += d[i] + d[i + 1] + d[i + 2]; b += d[i + 3]; n++; }"
    + "    return c.width + 'x' + c.height + ':' + a + ':' + b + ':' + n;"
    + "  }).join('|');"
    + "  let prev = null, same = 0;"
    + "  const deadline = performance.now() + " + SETTLE_DEADLINE_MS + ";"
    + "  while (performance.now() < deadline) {"
    + "    const now = probe();"
    + "    if (prev !== null && now === prev) { same++; if (same >= 1) return {ok: true, polls: same}; }"
    + "    else same = 0;"
    + "    prev = now;"
    + "    await new Promise((r) => setTimeout(r, " + SETTLE_POLL_MS + "));"
    + "  }"
    + "  return {ok: false, polls: same};"
    + "})()",
  awaitPromise: true,
  returnByValue: true,
});
// Un raster qui ne se stabilise pas est un FAIT, consigné, pas une erreur : une
// page à animation perpétuelle doit se voir dans la référence plutôt que faire
// tomber le filet. Ce qui serait inacceptable, c'est de l'ignorer.
const rasterSettled = settled?.result?.value?.ok === true;

const probed = await page.cmd("Runtime.evaluate", {
  expression: "(function () {"
    + "  const GRID = " + GRID + ", SHIFT = " + (8 - QUANT_BITS) + ";"
    // Un sélecteur STABLE et lisible pour nommer chaque canevas : le chemin de
    // ses ancêtres avec leur rang. Un identifiant engendré par le cadriciel
    // changerait à chaque montage et ferait bouger la référence sans que rien
    // n'ait bougé.
    + "  const pathOf = function (el) {"
    + "    const parts = [];"
    + "    for (let n = el; n && n.nodeType === 1 && n !== document.documentElement; n = n.parentElement) {"
    + "      const tag = n.tagName.toLowerCase();"
    + "      const sibs = n.parentElement ? Array.prototype.filter.call(n.parentElement.children, function (c) { return c.tagName === n.tagName; }) : [n];"
    + "      parts.unshift(sibs.length > 1 ? tag + '[' + (sibs.indexOf(n) + 1) + ']' : tag);"
    + "    }"
    + "    return parts.join('>');"
    + "  };"
    + "  const chartOf = function (cv) {"
    // Enrichissement OPTIONNEL : si une bibliothèque de graphiques est attachée
    // à ce canevas, on lit ce qu'elle DÉCLARE dessiner. Absente, la lane reste
    // entière — elle mesure des pixels, ce qui ne suppose aucune bibliothèque.
    // Les deux API connues sont interrogées, parce que la version majeure de la
    // bibliothèque est précisément ce qu'un lot peut changer.
    + "    if (!window.Chart) return null;"
    + "    let inst = null;"
    + "    try {"
    + "      if (typeof Chart.getChart === 'function') inst = Chart.getChart(cv);"
    + "      else if (Chart.instances) { for (const k in Chart.instances) { if (Chart.instances[k] && Chart.instances[k].canvas === cv) inst = Chart.instances[k]; } }"
    + "    } catch (e) { return {error: String(e)}; }"
    + "    if (!inst) return null;"
    + "    const cfg = inst.config || {};"
    + "    const data = inst.data || {};"
    + "    const norm = function (c) { return typeof c === 'string' ? c.replace(/\\s+/g, '').toLowerCase() : c; };"
    + "    return {"
    + "      version: (Chart.version || (Chart.defaults && Chart.defaults.global && '2.x') || '?'),"
    + "      type: cfg.type || (cfg._config && cfg._config.type) || '?',"
    + "      labels: (data.labels || []).map(String),"
    + "      datasets: (data.datasets || []).map(function (d) {"
    + "        return {"
    + "          label: d.label === undefined ? null : String(d.label),"
    + "          data: (d.data || []).map(function (v) { return v && typeof v === 'object' ? JSON.stringify(v) : v; }),"
    + "          colors: [].concat(d.backgroundColor || []).map(norm)"
    + "        };"
    + "      })"
    + "    };"
    + "  };"
    + "  const out = [];"
    + "  const list = Array.prototype.slice.call(document.querySelectorAll('canvas'));"
    + "  for (const cv of list) {"
    + "    const rect = cv.getBoundingClientRect();"
    // RENDU ou NON : la distinction décide de tout le reste. Une application
    // sert souvent PLUSIEURS panneaux dans le même document et n'en affiche
    // qu'un, choisi par l'URL ou par un onglet — les autres restent en
    // `display: none`. Un canevas non rendu mesure 0×0 et ne peut rien peindre,
    // ce qui est normal et n'est PAS le cas aveugle.
    //
    // Sans cette distinction, la lane rapporterait « n canevas vides » sur
    // chaque page, pour toujours, et ne pourrait plus jamais distinguer un
    // graphique CASSÉ d'un graphique CACHÉ : une lane aveugle déguisée en lane
    // voyante. Mesuré sur une cible réelle : la première version de cette lane
    // visait le seul panneau qui ne portait AUCUN graphique, et rendait un
    // résultat stable, plausible et à propos de rien. `getClientRects()` est vide pour un élément non rendu, ce qui est
    // la question posée — et non « quelle est sa taille », qu'un élément rendu
    // hors écran rendrait aussi.
    + "    const rec = {"
    + "      selector: pathOf(cv),"
    + "      rendered: cv.getClientRects().length > 0,"
    + "      backing: cv.width + 'x' + cv.height,"
    + "      css: Math.round(rect.width) + 'x' + Math.round(rect.height),"
    + "      chart: chartOf(cv)"
    + "    };"
    + "    const ctx = cv.getContext('2d');"
    + "    if (!ctx) { rec.error = 'pas de contexte 2d'; out.push(rec); continue; }"
    + "    if (!cv.width || !cv.height) { rec.error = 'canevas de taille nulle'; out.push(rec); continue; }"
    + "    const img = ctx.getImageData(0, 0, cv.width, cv.height).data;"
    + "    let inked = 0;"
    + "    const hist = new Map();"
    + "    const cells = new Array(GRID * GRID);"
    + "    for (let i = 0; i < cells.length; i++) cells[i] = [0, 0, 0, 0];"
    + "    for (let y = 0; y < cv.height; y++) {"
    + "      const gy = Math.min(GRID - 1, Math.floor(y * GRID / cv.height));"
    + "      for (let x = 0; x < cv.width; x++) {"
    + "        const p = (y * cv.width + x) * 4;"
    + "        const a = img[p + 3];"
    + "        const gx = Math.min(GRID - 1, Math.floor(x * GRID / cv.width));"
    + "        const cell = cells[gy * GRID + gx];"
    + "        cell[0] += img[p]; cell[1] += img[p + 1]; cell[2] += img[p + 2]; cell[3]++;"
    + "        if (a > 0) {"
    + "          inked++;"
    + "          const key = ((img[p] >> SHIFT) << (2 * " + QUANT_BITS + ")) | ((img[p + 1] >> SHIFT) << " + QUANT_BITS + ") | (img[p + 2] >> SHIFT);"
    + "          hist.set(key, (hist.get(key) || 0) + 1);"
    + "        }"
    + "      }"
    + "    }"
    + "    const total = cv.width * cv.height;"
    + "    let sig = '';"
    + "    for (const c of cells) {"
    + "      const n = c[3] || 1;"
    + "      sig += ((Math.round(c[0] / n) >> SHIFT).toString(16))"
    + "           + ((Math.round(c[1] / n) >> SHIFT).toString(16))"
    + "           + ((Math.round(c[2] / n) >> SHIFT).toString(16));"
    + "    }"
    + "    const top = Array.from(hist.entries()).sort(function (a, b) { return b[1] - a[1] || a[0] - b[0]; }).slice(0, 8);"
    + "    rec.inked = inked;"
    + "    rec.total = total;"
    + "    rec.distinct = hist.size;"
    + "    rec.raster = sig;"
    + "    rec.top = top.map(function (e) {"
    + "      const k = e[0];"
    + "      const r = (k >> (2 * " + QUANT_BITS + ")) & 0xf, g = (k >> " + QUANT_BITS + ") & 0xf, b = k & 0xf;"
    + "      return {color: r.toString(16) + g.toString(16) + b.toString(16), share: Math.round(e[1] * 1000 / total)};"
    + "    });"
    + "    out.push(rec);"
    + "  }"
    + "  return JSON.stringify({canvases: out, grid: GRID, quantBits: " + QUANT_BITS + "});"
    + "})()",
  returnByValue: true,
});

const value = probed?.result?.value;
// Une sonde qui ne rend rien doit CRIER. Sortie vide, elle se canonicaliserait
// en « aucun canevas » : le plus paisible des résultats, et une lane
// définitivement aveugle — exactement ce que cette lane existe pour empêcher.
if (typeof value !== "string" || !value.length) {
  page.fail("la sonde n'a rien rendu sur " + url + " — résultat brut : " + JSON.stringify(probed).slice(0, 400));
}
let parsed;
try { parsed = JSON.parse(value); } catch (e) { page.fail("sonde illisible : " + e); }
parsed.viewport = VIEWPORT.width + "x" + VIEWPORT.height;
parsed.rasterSettled = rasterSettled;

await page.closeTarget();
page.ws.close();
emit(JSON.stringify(parsed));
