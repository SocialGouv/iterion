# Plan raffiné — iterion V1 ambitieuse, multi-modèles, parallèle, alignée sur goai

## Résumé
- La V1 doit assumer dès le départ l’objectif produit complet : orchestrer des recettes de raffinage de PR et de code review multi-modèles, avec phases parallèles, merges, boucles de conformité et benchmarking de recettes.
- Le développement restera découpé en phases promptables, mais le cahier des charges initial doit déjà intégrer tous les concepts structurants nécessaires à cette cible V1.
- `iterion` reste construit comme un cœur Go réutilisable par un CLI aujourd’hui et un futur service demain.
- Le DSL auteur reste strict et compilé vers une IR canonique ; l’IR demeure la seule source de vérité d’exécution et Mermaid une vue dérivée.
- La V1 doit supporter :
  - exécution séquentielle et parallélisme contrôlé
  - fan-out/fan-in borné avec join explicite
  - sessions fraîches ou héritées selon les nœuds
  - pauses humaines reprenables avec questions/réponses persistées
  - prompts paramétrables pour review, compliance, synthesis, human arbitration, act
  - plusieurs modèles sur un même workflow
  - boucles bornées et politiques de stop
  - artefacts nommés, auditables et réinjectables
  - checkpoints humain-dans-la-boucle plaçables à différents endroits du workflow
  - comparaison de recettes coût / latence / qualité
- Le runtime s’ancre sur les vraies interfaces `goai` :
  - modèles via `provider.LanguageModel`
  - texte via `goai.GenerateText`
  - structuré via `goai.GenerateObject[map[string]any]` + `WithExplicitSchema`
  - tools via `goai.Tool`
  - observabilité via `WithOnRequest`, `WithOnResponse`, `WithOnToolCall`, `WithOnStepFinish`

## Cas d’usage directeurs de la V1
- `pr_refine_single_model`
  - review -> plan -> act -> verify_compliance -> done/fail ou boucle.
- `pr_refine_dual_model_sequential`
  - Claude review, puis GPT review, puis merge, puis act, puis vérification.
- `pr_refine_dual_model_parallel`
  - Claude et GPT reviewent la PR en parallèle à partir d’un prompt de review paramétrable qui contient les règles de compliance.
  - Chacun produit son review et son plan.
  - Chaque modèle repart dans une nouvelle session pour comparer les deux plans et produire une synthèse.
  - Un merge final unifie les deux synthèses.
  - Un vérificateur de conformité juge le plan fusionné avec un prompt de compliance dédié.
  - Si le verdict n’est pas atteint, boucle alternée séquentielle Claude puis GPT sur le plan jusqu’à conformité.
  - Ensuite phase `act`, puis nouvelle review / compliance à partir du prompt initial.
  - Si la PR n’est toujours pas conforme, rebouclage au début de la recette.
- `recipe_benchmark`
  - exécuter plusieurs recettes sur la même PR pour comparer coût, qualité, nombre d’itérations et latence.
- `ci_fix_until_green`
  - diagnostic -> plan -> act -> rerun -> verify -> loop.

## Scénario de référence détaillé V1
- Nom de référence :
  - `pr_refine_dual_model_parallel_compliance`
- Fichier DSL de référence :
  - `examples/pr_refine_dual_model_parallel_compliance.iter`
- Objectif :
  - Raffiner une PR jusqu’à atteindre un niveau de qualité et de compliance jugé satisfaisant, en croisant plusieurs modèles, en réduisant les biais d’un seul modèle, et en permettant de comparer plusieurs recettes en coût, latence et efficacité.
- Intention produit :
  - Ce scénario doit être traité comme le workflow phare de la V1.
  - Même si sa livraison est séquencée par phases, il doit guider les décisions de DSL, d’IR, de runtime, de persistance, d’observabilité et de tooling dès le premier jour.
  - Le fichier DSL de référence doit être maintenu à jour avec les décisions de design et servir de fixture produit, de support de discussion et de futur cas de démonstration Mermaid.

### Inputs du scénario
- Contexte PR :
  - branche source et branche cible
  - diff git
  - fichiers modifiés
  - métadonnées PR si disponibles
- Contexte repo :
  - arborescence utile
  - fichiers de configuration pertinents
  - tests ou commandes de vérification disponibles
- Paramètres de recette :
  - modèle de review A, par exemple Claude
  - modèle de review B, par exemple GPT
  - modèle arbitre final de merge, configurable
  - modèle ou stratégie pour la phase `act`
  - budgets globaux et locaux
  - nombre maximal d’itérations de raffinement
  - nombre maximal de rebouclages globaux
- Prompt packs paramétrables :
  - prompt de review PR
  - prompt de compliance review
  - prompt de synthesis de plans
  - prompt de merge final
  - prompt d’arbitrage humain pour détecter les décisions techniques à clarifier
  - prompt d’action
  - prompt de vérification finale
- Policies :
  - policy tools
  - allowlist de commandes
  - stratégie de stop sur budget ou temps

### Outputs attendus
- Artefacts intermédiaires :
  - `claude_review`
  - `gpt_review`
  - `claude_plan`
  - `gpt_plan`
  - `claude_plan_synthesis`
  - `gpt_plan_synthesis`
  - `final_merged_plan`
  - `plan_compliance_verdict`
  - `technical_decision_assessment`
  - `human_question_pack`
  - `human_answers`
  - `clarified_plan`
  - `iteration_report`
- Artefacts d’action :
  - `applied_patch` ou `git_diff_after_act`
  - `command_results`
  - `test_results`
- Artefacts finaux :
  - `final_review_report`
  - `final_compliance_verdict`
  - `run_summary`
  - `recipe_metrics`

### Déroulé exhaustif du scénario
1. Préparation du contexte
   - Charger le diff de la PR, les fichiers concernés, l’état git, les règles de compliance et les paramètres de recette.
   - Construire un paquet d’entrée normalisé pour tous les nœuds de review afin d’assurer une comparaison équitable entre modèles.
   - Initialiser le budget global du run et les compteurs de boucles.
2. Review parallèle initiale
   - Lancer deux branches parallèles :
     - `claude_review`
     - `gpt_review`
   - Les deux nœuds partent en `session: fresh`.
   - Ils reçoivent les mêmes inputs de PR et le même prompt de review paramétré par les règles de compliance.
   - Chaque nœud produit une review structurée contenant au minimum :
     - verdict
     - liste de problèmes
     - sévérité
     - recommandations
     - confiance
3. Planification parallèle initiale
   - Chaque branche produit ensuite son propre plan de correction :
     - `claude_plan`
     - `gpt_plan`
   - Ces plans doivent être structurés, auditables et réutilisables dans les étapes suivantes.
   - Chaque plan doit référencer explicitement les problèmes qu’il cherche à corriger.
4. Synthèse croisée des plans
   - Démarrer deux nouvelles sessions propres :
     - `claude_plan_synthesis`
     - `gpt_plan_synthesis`
   - Chaque nœud reçoit les deux plans et les deux reviews comme artefacts d’entrée.
   - Chaque modèle doit :
     - comparer les deux plans
     - identifier convergences et divergences
     - signaler les oublis, risques et incohérences
     - produire une synthèse fusionnée
   - Le fait de redémarrer en `session: fresh` est une exigence importante du scénario pour éviter l’enfermement dans le raisonnement précédent du modèle.
5. Merge final des synthèses
   - Un nœud de merge final, par défaut confié à GPT mais configurable, reçoit :
     - `claude_plan_synthesis`
     - `gpt_plan_synthesis`
   - Il produit un `final_merged_plan`.
   - Ce plan doit être la référence de travail de la suite du workflow.
6. Vérification de compliance du plan
   - Un `judge` dédié évalue `final_merged_plan` à partir d’un prompt de compliance spécifique au plan.
   - Ce nœud produit un verdict structuré :
     - `approved`
     - `issues`
     - `blocking_reasons`
     - `recommended_fixes`
   - Si le plan n’est pas conforme, le workflow entre dans une boucle de raffinage bornée.
   - Si le plan est conforme, le workflow ne passe pas immédiatement à `act` : il traverse d’abord un point d’arbitrage humain optionnel.
7. Arbitrage humain optionnel avant `act`
   - Un nœud d’arbitrage dédié évalue si des décisions techniques structurantes doivent être éclaircies avant exécution.
   - Ce nœud s’appuie sur un prompt d’arbitrage humain configurable, distinct du prompt de compliance.
   - Il produit un artefact structuré indiquant :
     - s’il faut solliciter un humain
     - quelles décisions sont concernées
     - pourquoi elles sont bloquantes ou risquées
     - une à plusieurs questions concrètes à poser
   - Si aucune clarification n’est nécessaire, le workflow passe directement à `act`.
   - Sinon, le run entre dans un état de pause reprenable.
   - Un nœud humain dédié expose les questions, attend les réponses, persiste les réponses, puis permet au run de reprendre exactement à cet endroit.
   - Après reprise, un nœud de réintégration met à jour le plan avec les réponses humaines et renvoie le plan clarifié vers un nouveau contrôle de compliance avant `act`.
8. Boucle alternée de raffinage du plan
   - Le plan fusionné est raffiné séquentiellement en alternant les modèles :
     - itération impaire : Claude
     - itération paire : GPT
   - Chaque itération consomme :
     - le dernier plan fusionné
     - le dernier verdict de compliance
     - l’historique minimal d’artefacts utiles
   - Après chaque raffinage, le `judge` de compliance réévalue le nouveau plan.
   - La boucle s’arrête si :
     - le plan est approuvé
     - le nombre maximal d’itérations est atteint
     - le budget global ou local est dépassé
9. Phase `act`
   - Une fois le plan approuvé, un ou plusieurs nœuds d’action appliquent le plan au workspace.
   - La phase `act` peut inclure :
     - lecture de fichiers
     - édition ou patch
     - exécution de commandes
     - lancement de tests
     - collecte de diffs
   - Cette phase doit produire des artefacts vérifiables et rejouables.
10. Vérification finale de la PR corrigée
   - Une nouvelle phase de review vérifie le résultat final.
   - La vérification finale repart du prompt initial de review PR et des règles de compliance associées.
   - La stratégie par défaut de la V1 est de refaire la logique de review sur le code corrigé, afin de juger la PR finale sur le même standard que l’analyse initiale.
11. Décision finale ou rebouclage global
   - Si la PR corrigée est jugée conforme, le workflow se termine sur `done`.
   - Sinon, le workflow peut reboucler au début de la recette :
     - nouvelle review
     - nouveaux plans
     - nouvelle synthèse
     - nouvelle phase `act`
   - Ce rebouclage global doit être borné par un compteur séparé du compteur de raffinage du plan.

### Contraintes de runtime imposées par ce scénario
- Support du parallélisme borné pour lancer plusieurs reviews ou plans en même temps.
- Support d’un `join` explicite pour agréger les artefacts de plusieurs branches.
- Support de sessions fraîches pour les nœuds de synthèse et de merge.
- Support d’artefacts nommés versionnés, afin de savoir quel plan ou quel verdict est la dernière référence.
- Support de boucles locales et de rebouclages globaux.
- Support d’un état de pause `waiting_human_input` avec reprise exacte du run.
- Support d’un nœud humain qui ne consomme pas de modèle mais produit un artefact de réponses.
- Support d’un mécanisme de reprise qui réinjecte les réponses humaines sans perdre les artefacts et le contexte utiles.
- Support de budgets hiérarchiques :
  - budget global du run
  - budget par sous-phase
  - budget par nœud si nécessaire
- Support de plusieurs modèles hétérogènes dans un même workflow.
- Support de métriques comparables d’une recette à l’autre :
  - coût
  - latence
  - nombre d’appels modèle
  - nombre d’itérations
  - qualité finale ou verdict final

### Critères de réussite du scénario
- Le workflow doit pouvoir exécuter la recette complète sans intervention humaine.
- Les reviews, plans, synthèses, verdicts et résultats d’action doivent être persistés comme artefacts auditables.
- Le système doit permettre de comprendre :
  - ce qu’a dit chaque modèle
  - ce qui a été fusionné
  - pourquoi un plan a été rejeté
  - pourquoi une PR a été finalement jugée conforme ou non
- Le coût total, la durée et le nombre de boucles doivent être traçables.
- Le même scénario doit pouvoir être reconfiguré pour tester des recettes plus rapides, moins coûteuses ou plus strictes.

## Capacités V1 à intégrer dans le cahier des charges
- Support natif des graphes séquentiels et des sous-graphes parallèles contrôlés.
- Join explicite et déterministe pour agréger plusieurs branches.
- Sessions explicites par nœud :
  - `fresh` pour repartir sans historique de messages
  - `inherit` pour continuer une conversation
  - `artifacts_only` pour reconstruire un contexte propre à partir d’artefacts entrants
- Human-in-the-loop explicite :
  - détection LLM configurable du besoin d’arbitrage humain
  - nœud humain de pause / reprise
  - réintégration des réponses dans la suite du workflow
- Prompts paramétrables injectés via variables de workflow et presets de recette.
- Distinction claire entre :
  - review d’une PR
  - plan de correction
  - merge de reviews ou de plans
  - arbitrage humain sur décisions structurantes
  - vérification de compliance
  - phase d’action
- Artefacts nommés persistés et réutilisables :
  - review brute
  - plan
  - synthèse
  - verdict
  - questions humaines
  - réponses humaines
  - patch ou diff
  - rapport final
- Budgets et politiques de stop dès la V1 :
  - `max_iterations`
  - `max_parallel_branches`
  - `max_tokens`
  - `max_cost`
  - `max_duration`
- Comparaison de recettes dès la V1 :
  - une recette = workflow + presets + policies + métriques attendues
  - la comparaison peut rester CLI/file-based en V1, mais son modèle doit être prévu dès maintenant

## Changements clés à apporter au plan initial
- Ajouter une `Phase 0` dédiée au contrat produit V1 :
  - workflow phare `pr_refine_dual_model_parallel`
  - taxonomie des nœuds
  - sémantique des branches, joins et sessions
  - sémantique de pause / reprise humaine
  - format des artefacts et des événements
  - budgets et policies
  - diagnostics de compilation
- Étendre le DSL pour exprimer explicitement :
  - `entry`
  - `input` et `output`
  - `system` et `user`
  - `tools`
  - `session`
  - `timeout`
  - `budget`
  - `human` checkpoints
  - groupes parallèles et joins
- Rendre le passage de données explicite :
  - `with` construit l’input du nœud suivant
  - `with` peut référencer `vars`, `input`, `outputs`, `artifacts`, `branch_outputs`
  - pas de langage d’expression libre en V1
- Ajouter des nœuds ou capacités explicites pour :
  - `agent`
  - `judge`
  - `router`
  - `tool`
  - `human`
  - `join`
  - `done`
  - `fail`
- Définir un ordre de sélection des transitions stable :
  - ordre source conservé
  - une seule edge par défaut par groupe de sortie
  - ambiguïté de matching = erreur de compilation
- Définir la sémantique multi-entrée :
  - un `join` attend explicitement les branches requises
  - un nœud standard n’agrège pas implicitement plusieurs entrées
  - l’agrégation passe par un `join` ou un `merge agent` explicite
- Ajouter une policy explicite pour les side effects :
  - lecture/écriture workspace autorisées par défaut
  - `run_command` contrôlé par allowlist configurable, avec wildcard `*` possible
  - policy injectée par config/CLI/runtime, pas décrite dans le DSL
- Ajouter un modèle de recettes dès la V1 :
  - fichier workflow
  - presets de variables
  - prompt packs
  - budgets
  - stratégie d’évaluation

## Interfaces publiques et contrats à figer
- `Compiler`
  - `Parse([]byte) (*dsl.File, []Diagnostic)`
  - `Compile(*dsl.File, CompileOptions) (*ir.Workflow, []Diagnostic)`
- `Diagnostic`
  - `Code`, `Severity`, `Path`, `Line`, `Column`, `Message`, `Hint`
- `RunSpec`
  - `WorkflowName`, `RecipeName`, `Inputs`, `WorkingDir`, `Env`, `StoreDir`, `ToolPolicy`, `MCPServers`, `OutputMode`, `Budget`
- `RunResult`
  - `RunID`, `Status`, `TerminalNode`, `FinalOutput`, `Usage`, `Cost`, `StartedAt`, `FinishedAt`, `Artifacts`, `Metrics`, `PendingInteraction`
- `RunStore`
  - `CreateRun`, `AppendEvent`, `PutArtifact`, `GetRun`, `ListEvents`, `UpdateStatus`, `ListArtifacts`, `GetPendingInteraction`, `SubmitHumanAnswers`, `ResumeRun`
- `RecipeSpec`
  - `WorkflowRef`, `PresetVars`, `PromptPack`, `Budget`, `EvaluationPolicy`
- `PendingInteraction`
  - `InteractionID`, `RunID`, `NodeName`, `Kind`, `Questions`, `Instructions`, `Status`, `CreatedAt`, `AnsweredAt`
- `NodeExecutor` par type de nœud
  - `agent` et `judge` construisent des appels `goai`
  - `router` évalue les conditions sans appel LLM
  - `tool` invoque un outil directement
  - `human` émet une demande de clarification, pause le run, puis restitue les réponses au workflow lors de la reprise
  - `join` attend et matérialise un agrégat de branches
- `SessionMode`
  - `fresh`
  - `inherit`
  - `artifacts_only`
- `ModelRegistry`
  - résout `provider/model-id` en `provider.LanguageModel`
  - expose aussi `provider.ModelCapabilitiesOf(model)` pour vérifier tools, structured output et limites du provider
- `ToolRegistry`
  - normalise built-ins et MCP en `goai.Tool`
  - règle de collision : namespace MCP obligatoire, par exemple `mcp.<server>.<tool>`

## Sémantique d’exécution à verrouiller
- Un `run` = une invocation d’un workflow compilé ou d’une recette, avec ses inputs, ses budgets et sa policy d’exécution.
- Un `step` iterion = une visite de nœud qualifiée par un `run_id`, un `branch_id` et un `session_mode`.
- Un `llm_step` = une itération interne de `goai.GenerateText` pendant une boucle d’outils ; ce n’est pas un step iterion.
- Les branches parallèles sont des sous-exécutions sœurs d’un même run, partageant le même budget global mais des contextes d’exécution distincts.
- Les sessions sont isolées par défaut sur les nœuds de synthèse et de merge, afin de permettre des “nouvelles sessions” propres pour comparer deux plans.
- Les nœuds `human` mettent le run dans un statut suspendu explicite, par exemple `paused_waiting_human`.
- Un run repris repart du nœud humain ou du nœud immédiatement suivant selon le design interne retenu, mais sans rejouer les nœuds déjà validés en amont.
- Les réponses humaines sont persistées comme artefacts et référencées par l’IR/runtime comme de nouvelles entrées structurées.
- Le temps passé en pause humaine ne consomme pas de budget modèle ni de budget tools, mais doit rester mesuré séparément pour l’observabilité du run.
- Les compteurs de boucle sont incrémentés au moment où une edge portant `LoopName` est traversée.
- `done` et `fail` sont des nœuds terminaux explicites dans l’IR, avec statut final associé.
- Échec de schéma structuré :
  - si `goai.GenerateObject[map[string]any]` ne peut pas parser/valider, le nœud échoue
  - en V1, pas de réparation implicite cachée dans le runtime
  - la stratégie de correction doit être modélisée dans le workflow
- Sémantique du parallélisme V1 :
  - fan-out explicite
  - nombre de branches borné
  - join explicite
  - pas d’ordonnanceur distribué ni de parallélisme non borné
- Sémantique de merge V1 :
  - les branches produisent des artefacts nommés
  - le `join` rassemble les artefacts et métadonnées
  - un nœud `agent` de merge ou de synthèse consomme cet agrégat pour produire une sortie consolidée
- Les événements minimum à persister :
  - `run_started`
  - `branch_started`
  - `node_started`
  - `llm_request`
  - `llm_step_finished`
  - `tool_called`
  - `artifact_written`
  - `human_input_requested`
  - `run_paused`
  - `human_answers_recorded`
  - `run_resumed`
  - `join_ready`
  - `node_finished`
  - `edge_selected`
  - `budget_warning`
  - `run_finished`
  - `run_failed`

## Phases d’implémentation
1. Phase 0 : cadrage V1 complet et workflows phares
   - Finaliser le workflow cible `pr_refine_dual_model_parallel`.
   - Figer le vocabulaire DSL/IR pour branches, joins, sessions, checkpoints humains, artefacts, budgets et recettes.
   - Produire les fixtures de référence :
     - `pr_refine_single_model`
     - `pr_refine_dual_model_parallel`
     - `examples/pr_refine_dual_model_parallel_compliance.iter`
     - `recipe_benchmark`
     - `ci_fix_until_green`
2. Phase 1 : DSL + diagnostics
   - Lexer/parser indent-sensitive, AST, erreurs de parse précises, golden tests.
   - Introduire `entry`, `session`, `budget`, branches parallèles, joins, checkpoints humains et artefacts nommés.
3. Phase 2 : compilation + validation
   - AST -> IR, résolution env/prompts/schemas.
   - Validation des edges, loops, inputs/outputs, joins, checkpoints humains, budgets, sessions et recipes.
4. Phase 3 : runtime core linéaire, boucles et pause/reprise
   - moteur de base, store local file-backed, artefacts, transitions, compteurs de boucle, status de pause et reprise sur réponses humaines.
5. Phase 4 : scheduler de branches et joins
   - exécution parallèle contrôlée, suivi des branches, agrégation via `join`, propagation des budgets globaux.
6. Phase 5 : intégration goai
   - registry modèles, mapping options `WithSystem`, `WithMessages`, `WithTools`, `WithMaxSteps`, `WithTimeout`, `WithExplicitSchema`.
   - capture des hooks `goai` vers le modèle d’événements iterion.
7. Phase 6 : tools, policies et phase `act`
   - built-ins, MCP adapter, allowlist commandes, phase de mutation workspace et vérification outillée.
8. Phase 7 : recettes, benchmark et observabilité
   - support des `RecipeSpec`, métriques coût/latence/qualité, comparaison de runs, replay local en lecture.
9. Phase 8 : CLI + Mermaid
   - `run`, `validate`, `diagram`, sorties humain/JSON/JSONL, vues Mermaid compactes et détaillées.
   - les nœuds `human` et les points de pause/reprise doivent être rendus explicitement dans Mermaid.
10. Phase 9 : durcissement produit
   - cancel/timeouts, compatibilité de format, erreurs ergonomiques, tests end-to-end sur recettes phares.

## Plan de tests
- Parsing
  - DSL valides/invalides, indentation, mots-clés réservés, diagnostics positionnés, groupes parallèles, joins, sessions.
- Compilation
  - refs manquantes, nœuds inatteignables, ambiguïtés de transitions, boucles sans fallback, `with` incompatible avec `input`, join mal câblé.
- Runtime séquentiel
  - chemin linéaire, booléen, enum, boucle bornée, boucle épuisée, terminal `done`, terminal `fail`.
- Runtime parallèle
  - double review Claude/GPT en parallèle, join, merge, budgets partagés, branches en erreur, branche lente bloquant le join.
- Human-in-the-loop
  - détection du besoin de clarification
  - création d’une interaction en attente
  - pause du run
  - reprise avec une ou plusieurs réponses
  - réintégration des réponses dans le plan puis revalidation
- Sessions et artefacts
  - session fraîche de synthèse, héritage de session, relecture à partir d’artefacts uniquement, persistance et rechargement.
- Intégration goai
  - texte simple, structuré via `WithExplicitSchema`, tool loop avec `WithMaxSteps`, hooks transformés en events.
- Recettes
  - exécution d’une recette avec presets, comparaison de deux recettes, collecte de métriques coût/latence/itérations/verdict final.
- Outils
  - built-ins, refus de commande non allowlistée, MCP namespacé, erreurs d’outil persistées en events.
- End-to-end
  - `pr_refine_single_model`
  - `pr_refine_dual_model_parallel`
  - `examples/pr_refine_dual_model_parallel_compliance.iter`
  - `ci_fix_until_green`
  - rebouclage global après compliance non atteinte

## Hypothèses et lignes rouges
- Nom du document : `plans/plan-iterion-v2-platform-ready.md`.
- Tous les objectifs décrits ci-dessus font partie de la V1 cible, même si leur livraison sera échelonnée.
- La V1 n’est pas “minimaliste” sur les capacités ; elle est ambitieuse, mais bornée par des choix de runtime stricts et auditables.
- Les outputs structurés du DSL sont stockés en `json.RawMessage` et manipulés en `map[string]any`, pas en types Go générés.
- Le parallélisme V1 est contrôlé et explicite ; pas de parallélisme implicite, non borné ou distribué.
- Les checkpoints humains font partie du modèle natif de la V1 ; ils ne doivent pas être implémentés comme un hack externe au runtime.
- Les patterns `plan_then_act`, `review_loop`, `critic_loop`, `pr_refine_dual_model_parallel` vivent comme workflows et recettes de référence, pas comme sucre syntaxique spécial.
- Le futur service doit pouvoir réutiliser sans rupture : IR, RunStore, event model, RecipeSpec, ToolRegistry et NodeExecutors.
