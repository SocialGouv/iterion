# Comparaison V1 (Claude) vs V2 (Codex) — Kanban 2026-04-03

## Contexte du workflow

Le workflow `dual_model_plan_implement_review` a tourne en **75.9 min** avec **2 iterations** de la boucle plan->impl->review :

1. **Iteration 1** : Claude implemente (round-robin index 0) -> review echoue (18/20 selon Codex, 20/20 selon Claude) -> judge rejette
2. **Iteration 2** : Codex implemente (round-robin index 1) -> review passe (19-20/20) -> judge approuve -> **done**

---

## Metriques

| Metrique | V1 (Claude) | V2 (Codex) | Delta |
|----------|-------------|------------|-------|
| Lignes | 2 532 | 3 340 | +32% |
| Taille | ~84 KB | ~116 KB | +38% |
| Fonctions JS | 19 | 39 | +105% |
| Tokens consommes | 45 522 | 147 260 | x3.2 |
| Duree d'implementation | ~15 min | ~15 min | = |
| Verdict review | Rejete (2 FAIL) | Approuve | |

---

## Ajouts majeurs de V2 par rapport a V1

1. **Theme preload script** (lignes 7-18) — Evite le flash de theme au chargement via un script inline dans `<head>`
2. **Normalisation robuste des donnees** — 4 fonctions dediees :
   - `normalizeTags()`, `normalizeSubtasks()`, `normalizeTask()`, `normalizeState()`
3. **Inline editing complet** — 10 fonctions supplementaires :
   - `createTaskDraft()`, `getEditingDraft()`, `syncInlineDraftValue()`, `syncInlineMetaValue()`
   - `startInlineEdit()`, `focusInlineEditor()`, `cancelInlineEdit()`
   - `toggleInlineTag()`, `addInlineTag()`, `saveInlineEdit()`
   - `buildInlineTaskEditor()` — genere le formulaire d'edition inline directement dans la carte
4. **Gestion des subtasks inline** — `removeInlineSubtask()`, `addInlineSubtask()`
5. **Suppression de tache dediee** — `confirmDeleteTask()` (separee de la suppression de colonne)
6. **History snapshots** — `createHistorySnapshot()` pour un undo/redo plus fiable
7. **Reindex des taches** — `reindexColumnTasks()` pour maintenir l'ordre coherent
8. **Validation d'import renommee** — `validateImportSchema()` (plus explicite que `validateImport()`)

---

## Analyse qualitative

### 1. Robustesse / Defense en profondeur

**V1 (Claude)** fait confiance aux donnees :
- `Object.assign(task, p.changes)` dans `UPDATE_TASK` — aucune validation des champs, n'importe quelle propriete peut etre injectee
- `Storage.load()` fait une validation minimale (`parsed.version && Array.isArray(...)`) puis passe les donnees telles quelles
- Les tags/subtasks sont juste assignes sans normalisation (`p.tags || []`)

**V2 (Codex)** est systematiquement defensif :
- Chaque champ est valide individuellement dans `UPDATE_TASK` (`typeof changes.title === 'string'`)
- 4 fonctions de normalisation qui valident **chaque propriete** avec `Number.isFinite()`, `typeof === 'string'`, deduplication des tags par nom case-insensitive, etc.
- `IMPORT_STATE` passe par `normalizeState()` au lieu d'assigner directement
- `Utils.generateId()` utilise `crypto.randomUUID()` avec fallback, vs `Date.now() + Math.random()` dans V1

### 2. Securite

**V2** ajoute `Utils.escapeHTML()` et `Utils.normalizeColor()` (validation CSS via `CSS.supports`) — absents de V1. Point important pour eviter les XSS via les noms de tags/taches.

### 3. Architecture

Les deux partagent la meme structure `dispatch -> Actions -> Storage.save -> Render.all()`. Mais :

- **V2** ajoute `reindexColumnTasks()` appele systematiquement apres les mutations (DELETE_TASK, MOVE_TASK, BULK_MOVE, BULK_DELETE, DELETE_COLUMN) — V1 oublie de reindexer dans certains cas
- **V2** gere proprement le nettoyage d'etat orphelin (`editingInline`, `selectedTaskId`) lors des suppressions — V1 ne le fait pas dans DELETE_TASK
- **V2** utilise `Set` au lieu de `Array.includes()` dans les operations bulk (meilleure complexite)

### 4. UX : Inline Editing

Differenciateur fonctionnel majeur. V2 implemente un systeme complet d'edition in-place avec :
- Draft temporaire (pas de mutation directe du state)
- Focus management avec `requestAnimationFrame`
- Save/cancel/toggle tags/add-remove subtasks directement dans la carte
- V1 oblige l'utilisateur a passer par une modale pour toute modification

### 5. Persistence

**V2** ajoute :
- `STORAGE_VERSION = 2` comme constante explicite
- `SAVE_DEBOUNCE_MS = 300` (configurable) vs le debounce hardcode de V1
- Theme preload script dans `<head>` pour eviter le flash
- `structuredClone` avec fallback JSON (plus fiable pour les types complexes)
- `createHistorySnapshot` normalise les timers avant snapshot (evite les bugs de undo avec des `runningSince` fantomes)

### 6. Ce que V1 fait mieux

- **Lisibilite** : code plus compact, plus facile a comprendre d'un coup d'oeil
- **Ratio signal/bruit** : pas de sur-ingenierie, chaque ligne a un but clair
- Si les 20 criteres n'exigeaient pas l'inline editing, V1 serait un meilleur point de depart pour iterer

---

## Verdict

**V2 est objectivement de meilleure qualite** sur les axes robustesse, securite et completude fonctionnelle. Le code de V1 est plus elegant et lisible, mais il coupe des coins sur la validation des donnees et le nettoyage d'etat — des bugs latents qui se manifesteraient en production avec des donnees corrompues dans localStorage. V2 a l'approche "production-ready", V1 a l'approche "prototype propre".
