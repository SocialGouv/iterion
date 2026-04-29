# ADR-001 : Ajout du mode `round_robin` au Router

- **Statut** : Accepte
- **Date** : 2026-04-01
- **Auteurs** : devthejo
- **Contexte workflow** : `examples/dual_model_plan_implement_review.iter`

## Contexte

Le DSL Iterion v1 permet d'orchestrer des workflows multi-agents avec des primitives de base : `agent`, `judge`, `router`, `join`, `human`, `tool`. Le router ne supporte actuellement qu'un seul mode — `fan_out_all` — qui spawn toutes les branches sortantes en parallele.

Lorsqu'un workflow necessite d'**alterner entre deux agents** a chaque iteration d'une boucle (ex : Claude raffine au tour 1, Codex au tour 2), le DSL v1 impose un **pattern cross-pair** : dupliquer structurellement les noeuds et croiser les edges de rejet entre les deux paires.

Ce pattern a ete mis en evidence lors de la conception du workflow `dual_model_plan_implement_review.iter`, qui orchestre :
- Une planification parallele (Claude + Codex) avec fusion par Claude
- Une boucle de validation/raffinage avec alternance du raffineur
- Une implementation avec alternance de l'implementeur
- Une review parallele avec retour a la planification en cas de rejet

### Cout mesure du pattern cross-pair

| Metrique | Avec cross-pair | Avec `round_robin` (estime) |
|---|---|---|
| Noeuds | 46 | ~23 |
| Edges | 60 | ~30 |
| Lignes `.iter` | ~550 | ~280 |
| Prompts dupliques | 0 (partages) | 0 |
| Noeuds dupliques | 23 (tout sauf prompts/schemas) | 0 |

La duplication est purement structurelle — chaque noeud duplique a le meme delegate, les memes prompts, les memes schemas. Son seul role est de fournir un point d'ancrage pour des edges differents.

## Decision

Ajouter un mode `round_robin` au noeud `router` dans le DSL v1.

### Syntaxe

```iter
router refine_selector:
  mode: round_robin
```

### Semantique

- A chaque traversee du router, **un seul** edge sortant est active, selectionne par un compteur cyclique : `edge_index = counter % len(edges)`
- Le compteur est **auto-incremente** a chaque traversee
- Le compteur est **persiste** dans le run state (analogue aux `loopCounters`)
- Le compteur demarre a 0 (premier edge declare dans le workflow)
- En cas de `resume` apres pause, le compteur est restaure depuis le store

### Exemple d'usage

```iter
router refine_selector:
  mode: round_robin

agent claude_refine:
  delegate: "claude_code"
  ...

agent codex_refine:
  delegate: "codex"
  ...

workflow example:
  ...
  val_judge -> refine_selector when not ready as refine_loop(4)

  refine_selector -> claude_refine with { ... }
  refine_selector -> codex_refine with { ... }

  claude_refine -> val_fanout with { ... }
  codex_refine -> val_fanout with { ... }
  ...
```

Au premier passage : `claude_refine` est selectionne.
Au deuxieme passage : `codex_refine` est selectionne.
Au troisieme passage : `claude_refine` a nouveau. Etc.

### Workflow simplifie

Avec `round_robin`, le workflow `dual_model_plan_implement_review.iter` se reduit a :

```
plan_fanout (fan_out_all) → claude_plan + codex_plan → plans_join → merge_plans
  → val_fanout (fan_out_all) → claude_val + codex_val → val_join → val_judge
    → [ready] → impl_selector (round_robin) → claude_implement | codex_implement
    → [not ready] → refine_selector (round_robin) → claude_refine | codex_refine
      → val_fanout (boucle)
  → review_fanout (fan_out_all) → claude_review + codex_review → review_join → review_judge
    → [approved] → done
    → [not approved] → plan_fanout (boucle externe avec reviews)
```

23 noeuds, zero duplication, intention lisible.

## Alternatives considerees

### 1. Statu quo — pattern cross-pair uniquement

Le pattern cross-pair fonctionne et ne necessite aucune modification du runtime. Il est utilise dans plusieurs exemples existants (`todo_app_full_dual_model_delegate.iter`, `feature_request_dual_model.iter`).

**Rejete car** : la duplication croit de facon combinatoire. Une alternance a 2 agents double les noeuds. A 3 agents, le cross-pair produirait 3x les noeuds avec 6 chemins croises. A 4, l'explosion est ingerable. Le pattern ne scale pas.

### 2. Sub-workflows / macros

Encapsuler le pattern cross-pair dans un sub-workflow reutilisable pour masquer la duplication.

**Rejete car** : le DSL v1 ne supporte pas les sub-workflows. Les ajouter serait un changement bien plus lourd qu'un nouveau mode de router, avec des implications sur le scoping des variables, les artifacts, et le store. Disproportionne par rapport au probleme.

### 3. Router conditionnel avec etat utilisateur

Permettre au router d'evaluer une expression sur les outputs d'un noeud precedent pour choisir l'edge (ex : `mode: condition`, `when last_refiner == "claude" -> codex_refine`).

**Rejete car** : introduit un mini-langage d'expressions dans le DSL, complexifie le parsing et la validation, et le `round_robin` couvre le cas d'usage principal (alternance deterministe) plus simplement.

## Arguments en faveur

### 1. Reduction de surface drastique

La moitie du fichier `dual_model_plan_implement_review.iter` est du boilerplate structurel qui n'apporte rien au lecteur. Chaque noeud duplique a exactement le meme delegate, les memes prompts, les memes schemas — seul son nom differe pour ancrer des edges differents.

### 2. Lisibilite et intention declarative

Le cross-pair encode l'intention "alterner" de facon indirecte, via la structure du graphe. Un lecteur doit reconstituer mentalement le pattern pour comprendre qu'il s'agit d'une alternance. Avec `round_robin`, l'intention est explicite et declarative.

### 3. Maintenabilite

Avec le cross-pair, modifier un prompt, un schema, ou un mapping `with {}` necessite de repercuter le changement dans toutes les paires. Un oubli cree une divergence silencieuse entre paires. Avec `round_robin`, chaque noeud n'existe qu'une fois.

### 4. Composabilite

Le `round_robin` se combine naturellement avec les autres primitives :
- Avec des boucles bornees (`as loop(N)`) : l'alternance s'arrete quand la boucle expire
- Avec des `fan_out_all` en amont/aval : on peut alterner l'implementeur tout en parallelisant les reviewers
- Extension future a N agents sans explosion combinatoire

### 5. Nouveaux patterns rendus possibles

- Rotation d'equipe a 3+ agents
- Alternance implementeur/reviewer asymetrique
- Diversite de modeles sur des taches repetees (eviter le biais d'un seul modele)

## Arguments en defaveur

### 1. Introduction d'etat dans le router

Aujourd'hui, le router `fan_out_all` est **stateless** : il lit ses edges et les spawn. Le `round_robin` necessite un compteur persistant. Cela casse l'invariant "un noeud ne depend que de ses inputs et du graphe".

**Mitigation** : les `loopCounters` sont deja un etat persistant dans le runtime, gere et serialise de facon analogue. Le `roundRobinCounters` suit exactement le meme pattern — ce n'est pas un precedent, c'est une extension naturelle.

### 2. Semantique dans les boucles

Quand un `round_robin` est atteint via une boucle bornee, la question se pose : quand incrementer le compteur ? A chaque traversee ou a chaque cycle complet ?

**Resolution** : incrementer a chaque traversee, c'est la semantique la plus simple et la plus intuitive. Un cycle de boucle = une traversee = un increment. Le compteur est un entier monotone croissant, modulo N.

### 3. Determinisme et debugging

Le chemin d'execution depend de l'historique de traversee (le compteur), pas seulement des outputs des noeuds. Cela complique le debugging : "pourquoi codex a ete choisi ?" necessite d'inspecter l'etat interne du compteur.

**Mitigation** : emettre un evenement `router_selected` dans le log du run, indiquant l'edge choisi et la valeur du compteur. L'outil `inspect --events` rend cette information visible.

### 4. Validation du graphe plus complexe

Le compilateur IR doit verifier des contraintes supplementaires pour `round_robin` :
- Au moins 2 edges sortants (sinon c'est un noeud normal)
- Les schemas d'input des cibles doivent etre compatibles (meme `with {}` alimente N cibles)

**Mitigation** : ces validations sont simples a implementer et suivent le modele existant de `pkg/dsl/ir/validate.go`.

### 5. Risque de feature creep

Apres `round_robin`, on voudra `weighted_round_robin`, `random`, `least_recently_used`...

**Mitigation** : limiter v1 a `fan_out_all` et `round_robin`. Les modes avances sont des extensions futures explicitement hors scope. Le type `RouterMode` est deja un enum extensible.

## Plan d'implementation

### Fichiers impactes

| Fichier | Modification |
|---|---|
| `grammar/iterion_v1.ebnf` | Ajouter `round_robin` a la regle `router_mode` |
| `grammar/V1_SCOPE.md` | Documenter le nouveau mode |
| `pkg/dsl/ast/ast.go` | Ajouter `RouterModeRoundRobin` a l'enum `RouterMode` |
| `pkg/dsl/parser/` | Parser `round_robin` comme valeur de `mode:` |
| `pkg/dsl/ir/ir.go` | Ajouter `RouterRoundRobin` au type `RouterMode` IR |
| `pkg/dsl/ir/compile.go` | Compiler le mode AST vers IR |
| `pkg/dsl/ir/validate.go` | Valider >= 2 edges sortants, schemas compatibles |
| `pkg/runtime/engine.go` | Selection d'edge par `counter % len(edges)` dans `execRouter` / `findNext` |
| `pkg/store/` | Serialiser/deserialiser `roundRobinCounters` dans le run state |
| `pkg/cli/diagram.go` | Representation visuelle distincte pour `round_robin` |

### Structure de l'etat

```go
// Dans RunState ou equivalent
type RunState struct {
    // ... champs existants ...
    LoopCounters       map[string]int  // existant
    RoundRobinCounters map[string]int  // nouveau — cle: nodeID du router
}
```

### Logique runtime (pseudo-code)

```go
func (e *Engine) execRouter(ctx context.Context, rs *RunState, nodeID string) (string, error) {
    node := e.workflow.Graph.Nodes[nodeID]
    edges := e.workflow.Graph.EdgesFrom(nodeID)

    switch node.RouterMode {
    case ir.RouterFanOutAll:
        return e.execFanOut(ctx, rs, nodeID)

    case ir.RouterRoundRobin:
        counter := rs.RoundRobinCounters[nodeID]
        selectedEdge := edges[counter % len(edges)]
        rs.RoundRobinCounters[nodeID] = counter + 1
        // Resoudre les inputs via le with{} de l'edge selectionnee
        // Executer le noeud cible
        return selectedEdge.Target, nil
    }
}
```

### Tests

- **Unitaire** : parser `round_robin`, compiler, valider (>= 2 edges, < 2 edges = erreur)
- **Integration** : workflow minimal avec `round_robin` a 2 cibles, verifier l'alternance sur 4 iterations
- **E2E** : workflow avec `round_robin` + boucle bornee + resume, verifier la persistance du compteur
- **Regression** : s'assurer que `fan_out_all` est inchange

### Migration du workflow existant

Une fois le `round_robin` implemente, le workflow `dual_model_plan_implement_review.iter` pourra etre simplifie de 46 a 23 noeuds. Les exemples existants utilisant le cross-pair pattern (`todo_app_full_dual_model_delegate.iter`, `feature_request_dual_model.iter`) restent valides — le cross-pair est un pattern d'usage, pas une contrainte du DSL.

## Consequences

- Le DSL v1 gagne une primitive de routage qui couvre un cas d'usage frequent (alternance d'agents) sans recourir a la duplication structurelle
- Le runtime gagne un vecteur d'etat supplementaire (`roundRobinCounters`) a persister et restaurer
- Les workflows futurs pourront exprimer des patterns d'alternance de facon declarative et concise
- Le pattern cross-pair reste disponible pour les cas ou un controle plus fin est necessaire
- Le type `RouterMode` est prepare pour des extensions futures (`weighted`, `random`, etc.) sans changement d'architecture

---

## Addendum (2026-04-28) — Recommandations de backend

Le pattern `round_robin` decrit ci-dessus reste pleinement valide. En revanche, le choix initial d'illustrer l'alternance avec **Claude Code + Codex** n'est plus recommande : depuis cette ADR, l'experience accumulee a montre que le backend `codex` souffre de limitations significatives (impossibilite de configurer son set d'outils, tendance a remplir lui-meme sa fenetre de contexte, integration moins aboutie). Le compilateur emet desormais un warning `C030` lorsqu'un noeud utilise `backend: "codex"`.

Pour les nouveaux workflows utilisant `round_robin`, preferer une alternance entre :
- `claude_code` (delegate) + `claw` direct API avec un modele OpenAI (`model: "openai/gpt-5.4-mini"`), ou
- deux instances de `claude_code` configurees avec des modeles Claude differents (e.g. Sonnet vs Opus), ou
- deux modeles directs via `claw` (e.g. `anthropic/claude-...` vs `openai/gpt-...`).

Les examples historiques qui utilisaient `codex` dans ce role ont ete migres dans le meme commit ; voir `examples/dual_model_plan_implement_review.iter` pour la version courante.
