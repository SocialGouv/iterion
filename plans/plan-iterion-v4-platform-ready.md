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
  - `inherit` pour continuer la conversation du nœud parent direct (le prédécesseur immédiat dans le graphe). Seul l’historique de messages du parent direct est transmis — pas la branche entière.
  - `artifacts_only` pour reconstruire un contexte propre à partir d’artefacts entrants via `with`
  - **Contrainte de compilation** : `session: inherit` est interdit sur un nœud situé immédiatement après un `join`. Après un join, seuls `fresh` ou `artifacts_only` sont autorisés, car il n’existe pas de conversation unique à hériter depuis plusieurs branches.
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
- Artefacts nommés persistés, versionnés et réutilisables :
  - review brute
  - plan
  - synthèse
  - verdict
  - questions humaines
  - réponses humaines
  - patch ou diff
  - rapport final
  - **Versionning en boucle** : quand un nœud s'exécute plusieurs fois dans une boucle, chaque exécution produit une nouvelle version de son artefact. `outputs.<node>` retourne toujours la dernière version. L'historique des versions est accessible via `outputs.<node>.history` (tableau ordonné, index 0 = première itération). Cela permet par exemple à un judge de comparer le plan courant avec un plan précédent.
- Budgets et politiques de stop dès la V1 :
  - `max_iterations`
  - `max_parallel_branches`
  - `max_tokens`
  - `max_cost_usd`
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
  - `tool_max_steps`
  - `session`
  - `timeout`
  - `budget`
  - `human` checkpoints
  - groupes parallèles et joins
  - propriétés spécifiques par type de nœud, par exemple `mode`, `strategy`, `require`, `instructions`, `min_answers`
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
  - pour un nœud standard, la première edge conditionnelle vraie gagne ; si aucune condition ne matche, l’unique edge par défaut est prise
  - plusieurs edges conditionnelles vraies sur un nœud standard = erreur de compilation
  - plusieurs edges inconditionnelles sortantes hors `router mode: fan_out_all` = erreur de compilation
  - ambiguïté de matching = erreur de compilation
- Définir la sémantique minimale des conditions V1 :
  - sur les edges d’un nœud standard, `when <field>` et `when not_<field>` ne ciblent que des champs booléens du output structuré du nœud source
  - pas de mini langage d’expression sur les edges en V1
  - les routages plus riches passent soit par un `router mode: condition`, soit par un nœud `agent`/`judge` produisant une sortie structurée dédiée
- Définir la sémantique multi-entrée :
  - un `join` attend explicitement les branches requises
  - un nœud standard n’agrège pas implicitement plusieurs entrées
  - l’agrégation passe par un `join` ou un `merge agent` explicite
- Déclarer explicitement la publication d’artefacts persistants :
  - le DSL doit offrir un mécanisme pour publier un output comme artefact persistant nommé
  - le choix exact de syntaxe peut être tranché en phase 0, mais `artifacts.<name>` ne doit pas dépendre d’une convention implicite du runtime
- Ajouter une policy explicite pour les side effects :
  - lecture/écriture workspace autorisées par défaut
  - `run_command` contrôlé par allowlist configurable, avec wildcard `*` possible
  - sur un même `WorkingDir`, une seule branche mutante du workspace est autorisée à la fois en V1 ; une branche mutante = une branche contenant au moins un nœud ou tool susceptible d’écrire dans le repo ou d’en changer l’état observable (`write_file`, `patch`, `run_command`, etc.)
  - le parallélisme vise d’abord les branches de review/synthèse et les branches read-only
  - `recipe_benchmark` et la comparaison de recettes doivent s’exécuter sur des workspaces isolés par run (clone temporaire, worktree ou snapshot), jamais sur un workspace mutable partagé
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
  - `router` : strictement déterministe en V1. Deux modes : `fan_out_all` (lance toutes les branches sortantes en parallèle) et `condition` (évalue des conditions statiques sur les inputs pour choisir une branche). Aucun appel LLM. Le routage nécessitant un LLM passe par un nœud `agent` suivi d'edges conditionnelles sur son output structuré.
  - `tool` : invoque un outil directement sans appel LLM. Produit un artefact à partir du résultat. Cas d'usage : envoyer un rapport, déclencher un webhook, exécuter une commande de build, collecter un diff brut. Le nœud `tool` a un `input`, un `output` (schema de l'artefact produit), et une référence à un tool du `ToolRegistry`.
  - `human` émet une demande de clarification, pause le run, puis restitue les réponses au workflow lors de la reprise
  - `join` attend et matérialise un agrégat de branches. Stratégie configurable : `wait_all` (défaut) ou `best_effort`.
- `SessionMode`
  - `fresh` : aucun historique de messages, contexte reconstruit uniquement à partir des inputs `with`
  - `inherit` : historique de messages du nœud parent direct (prédécesseur immédiat dans le graphe). Interdit après un `join` (erreur de compilation).
  - `artifacts_only` : pas d'historique de messages, mais les artefacts référencés via `with` sont injectés comme contexte structuré
- `ArtifactRef`
  - `outputs.<node>` : dernière valeur produite par le nœud
  - `outputs.<node>.history` : tableau ordonné de toutes les versions (index 0 = première itération)
  - `artifacts.<name>` : artefacts marqués `persistent`, survivent aux reloops globaux
  - `vars.<name>` : variables du workflow
  - `input.<field>` : champs de l'input du nœud courant (transmis par le `with` de l'edge entrante)
- `JoinStrategy`
  - `wait_all` (défaut) : toutes les branches doivent réussir, sinon le join propage l'erreur
  - `best_effort` : le join agrège les résultats des branches réussies, les branches échouées sont marquées dans les métadonnées
- `RetryPolicy`
  - `MaxAttempts` : nombre maximal de tentatives sur erreur LLM transitoire (défaut : 3)
  - `BackoffBase` : durée de base pour le backoff exponentiel (défaut : 1s)
  - `RetryableErrors` : rate limit (429), timeout, erreur serveur (5xx). Les erreurs 4xx hors rate limit ne sont pas retryées.
- `ToolNodeSpec`
  - `ToolRef` : référence à un tool du `ToolRegistry`
  - `Input` : schema d'entrée
  - `Output` : schema de l'artefact produit
  - Pas d'appel LLM. Exécution directe du tool avec les inputs résolus.
- `ModelRegistry`
  - résout `provider/model-id` en `provider.LanguageModel`
  - expose aussi `provider.ModelCapabilitiesOf(model)` pour vérifier tools, structured output et limites du provider
- `ToolRegistry`
  - normalise built-ins et MCP en `goai.Tool`
  - règle de collision : namespace MCP obligatoire, par exemple `mcp.<server>.<tool>`

## Contrats opérationnels V1 à figer
- Persistance locale file-backed minimale :
  - `runs/<run_id>/run.json`
  - `runs/<run_id>/events.jsonl`
  - `runs/<run_id>/artifacts/<node>/<version>.json`
  - `runs/<run_id>/interactions/<interaction_id>.json`
  - ce layout peut évoluer plus tard, mais il doit être stable et documenté en V1 pour permettre audit, replay et debugging
- CLI minimale pour rendre le modèle opérable :
  - `iterion run <workflow-or-recipe>`
  - `iterion validate <file>`
  - `iterion diagram <file>`
  - `iterion inspect <run_id>`
  - `iterion resume <run_id> --answers-file <file>` ou équivalent non interactif
- Contrat d’évaluation des recettes :
  - `EvaluationPolicy` doit expliciter une métrique primaire de réussite, par exemple verdict final `approved`
  - les métriques secondaires minimales sont : coût total, durée totale, nombre d’itérations de boucle, nombre de retries, statut final
  - si une mesure de “qualité” plus riche est voulue, elle doit être définie explicitement par recette ; pas de score implicite ou magique dans le runtime

## Sémantique d’exécution à verrouiller
- Un `run` = une invocation d’un workflow compilé ou d’une recette, avec ses inputs, ses budgets et sa policy d’exécution.
- Un `step` iterion = une visite de nœud qualifiée par un `run_id`, un `branch_id` et un `session_mode`.
- Un `llm_step` = une itération interne de `goai.GenerateText` pendant une boucle d’outils ; ce n’est pas un step iterion.
- Sélection des transitions sur un nœud standard :
  - les edges sortantes sont évaluées dans l’ordre source
  - la première condition vraie gagne
  - s’il n’existe aucune condition vraie, l’unique edge sans `when` sert de fallback
  - hors `router mode: fan_out_all`, un nœud standard ne peut jamais sélectionner plusieurs edges sortantes sur une même exécution
- Les branches parallèles sont des sous-exécutions sœurs d’un même run, partageant le même budget global en mode first-come-first-served, avec des contextes d’exécution distincts.
  - Le budget global est partagé sans pré-allocation par branche. Chaque nœud consomme du budget global au fur et à mesure.
  - Un event `budget_warning` est émis quand un seuil est franchi (par exemple 80% du budget consommé).
  - Si le budget global est épuisé, les nœuds en cours terminent leur step LLM courant puis échouent avec une erreur `budget_exceeded`.
- Sémantique de mutation du workspace :
  - les nœuds read-only peuvent s’exécuter en parallèle
  - les nœuds ou tools susceptibles d’écrire dans le workspace ne doivent pas s’exécuter en parallèle sur le même `WorkingDir` en V1
  - une branche mutante peut donc coexister avec des branches read-only si le runtime garantit qu’elles ne lisent pas un état intermédiaire instable ; par défaut V1 peut choisir une règle plus simple et plus sûre : aucune lecture concurrente pendant une phase de mutation
  - une recette de benchmark doit disposer de son propre workspace isolé pour éviter toute interférence entre runs
- Les sessions sont isolées par défaut sur les nœuds de synthèse et de merge, afin de permettre des “nouvelles sessions” propres pour comparer deux plans.
- Les nœuds `human` mettent le run dans un statut suspendu explicite, par exemple `paused_waiting_human`.
- Un run repris repart du nœud humain ou du nœud immédiatement suivant selon le design interne retenu, mais sans rejouer les nœuds déjà validés en amont.
- Les réponses humaines sont persistées comme artefacts et référencées par l’IR/runtime comme de nouvelles entrées structurées.
- Le temps passé en pause humaine ne consomme pas de budget modèle ni de budget tools, mais doit rester mesuré séparément pour l’observabilité du run.
- Les compteurs de boucle sont incrémentés au moment où une edge portant `LoopName` est traversée.
- Sémantique du reloop global :
  - Quand le workflow reboucle au début (ex: `final_pr_compliance_check -> context_builder`), chaque cycle global repart d’une **ardoise vierge** côté `outputs` : les artefacts des cycles précédents ne sont plus accessibles via `outputs.<node>`.
  - C’est intentionnel : après un `act`, le workspace a changé, le contexte PR a changé, et les anciennes reviews/plans ne sont plus pertinents.
  - **Artifacts persistants** : certains artefacts peuvent être marqués comme `persistent` dans le workflow. Ces artefacts survivent aux reloops globaux et restent accessibles via `artifacts.<name>`. Cela permet par exemple de conserver un `run_summary` cumulatif ou un historique de décisions humaines entre les cycles.
  - Les compteurs de boucle globaux (`full_recipe_loop`) sont incrémentés à chaque reloop et ne sont pas remis à zéro.
  - Les compteurs de boucle locaux (`plan_refine_loop`) sont remis à zéro à chaque nouveau cycle global.
- `done` et `fail` sont des nœuds terminaux explicites dans l’IR, avec statut final associé.
- Échec de schéma structuré :
  - si `goai.GenerateObject[map[string]any]` ne peut pas parser/valider, le nœud échoue
  - en V1, pas de réparation implicite cachée dans le runtime
  - la stratégie de correction doit être modélisée dans le workflow
- Stratégie d’erreur runtime :
  - **Erreur LLM transitoire** (rate limit, timeout réseau, erreur serveur) : le runtime effectue un retry automatique borné (3 tentatives, backoff exponentiel). Si les 3 tentatives échouent, le nœud échoue. Les retries sont comptabilisés dans les events mais ne consomment pas d’itération de boucle workflow.
  - **Erreur tool** (exit code != 0, timeout) : le résultat d’erreur est renvoyé au LLM dans sa tool loop pour qu’il s’adapte (correction de commande, stratégie alternative). Le LLM décide de la suite dans les limites de son `tool_max_steps`. Si le LLM ne parvient pas à résoudre l’erreur dans ses steps, le nœud échoue.
  - **Échec d’un nœud dans une branche parallèle** : le comportement dépend de la stratégie du `join` :
    - `wait_all` (défaut) : toutes les branches doivent réussir. Si une branche échoue, les autres branches en cours continuent jusqu’à complétion, puis le join propage l’erreur.
    - `best_effort` : le join attend toutes les branches, agrège les résultats des branches réussies, et marque les branches échouées dans les métadonnées. Le workflow continue avec les résultats partiels.
    - La stratégie est configurable sur chaque `join` dans le DSL.
- Sémantique du parallélisme V1 :
  - fan-out explicite
  - nombre de branches borné
  - join explicite avec stratégie configurable (`wait_all` | `best_effort`)
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
  - `llm_retry` (tentative de retry après erreur transitoire, avec numéro de tentative et erreur d'origine)
  - `llm_step_finished`
  - `tool_called`
  - `tool_error` (erreur tool renvoyée au LLM pour adaptation)
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
   - `run`, `validate`, `diagram`, `inspect`, `resume`, sorties humain/JSON/JSONL, vues Mermaid compactes et détaillées.
   - les nœuds `human` et les points de pause/reprise doivent être rendus explicitement dans Mermaid.
10. Phase 9 : durcissement produit
   - cancel/timeouts, compatibilité de format, erreurs ergonomiques, tests end-to-end sur recettes phares.

## Critères de sortie par phase
1. Phase 0 est terminée quand le workflow de référence, la taxonomie des nœuds, la sémantique de transition et le contrat de persistance sont figés noir sur blanc.
2. Phase 1 est terminée quand le parser accepte les fixtures de référence et produit des diagnostics stables, positionnés et testés.
3. Phase 2 est terminée quand `examples/pr_refine_dual_model_parallel_compliance.iter` compile vers une IR valide et que les erreurs de câblage principales sont rejetées statiquement.
4. Phase 3 est terminée quand un run séquentiel avec boucle et pause/reprise humaine fonctionne de bout en bout sur store file-backed.
5. Phase 4 est terminée quand le scénario de review parallèle avec `join` s’exécute avec budgets partagés et comportement d’erreur conforme à la stratégie du `join`.
6. Phase 5 est terminée quand les appels `goai` sont branchés avec outputs structurés validés, tool loops bornées et hooks remontés en events iterion.
7. Phase 6 est terminée quand une phase `act` peut modifier le workspace de façon contrôlée, produire diff/artefacts/tests et faire respecter la policy tools.
8. Phase 7 est terminée quand au moins deux recettes peuvent être exécutées sur des workspaces isolés et comparées via des métriques persistées et relisibles.
9. Phase 8 est terminée quand le CLI couvre `run`, `validate`, `diagram`, `inspect`, `resume` et que Mermaid rend les branches, joins et nœuds humains sans perte d’information critique.
10. Phase 9 est terminée quand les recettes phares passent en end-to-end avec cancel, timeout, reprise humaine et compatibilité de format vérifiés.

## Plan de tests
- Parsing
  - DSL valides/invalides, indentation, mots-clés réservés, diagnostics positionnés, groupes parallèles, joins, sessions.
- Compilation
  - refs manquantes, nœuds inatteignables, ambiguïtés de transitions, boucles sans fallback, `with` incompatible avec `input`, join mal câblé.
  - `session: inherit` après un `join` doit produire une erreur de compilation.
  - référence à `outputs.<node>.history` dans un `with` doit être validée (le nœud existe, est dans une boucle).
  - `when <field>` / `when not_<field>` ne doivent accepter que des champs booléens valides du output source.
  - plusieurs edges par défaut ou plusieurs edges conditionnelles vraies possibles sur un nœud standard doivent être rejetées.
- Runtime séquentiel
  - chemin linéaire, booléen, enum, boucle bornée, boucle épuisée, terminal `done`, terminal `fail`.
- Runtime parallèle
  - double review Claude/GPT en parallèle, join, merge, budgets partagés, branches en erreur, branche lente bloquant le join.
  - join `wait_all` avec une branche en erreur : les autres branches complètent, puis le join propage l'erreur.
  - join `best_effort` avec une branche en erreur : le workflow continue avec les résultats partiels.
  - budget first-come-first-served : une branche qui épuise le budget, l'autre reçoit `budget_exceeded`.
  - refus d’exécuter deux branches mutantes en parallèle sur le même workspace.
  - clarification : une branche de review parallèle est read-only ; une branche `act` qui applique des patches est mutante.
  - benchmark multi-recettes avec workspaces isolés et absence d’interférence entre runs.
- Human-in-the-loop
  - détection du besoin de clarification
  - création d’une interaction en attente
  - pause du run
  - reprise avec une ou plusieurs réponses
  - réintégration des réponses dans le plan puis revalidation
- Sessions et artefacts
  - session fraîche de synthèse, héritage de session (parent direct uniquement), relecture à partir d’artefacts uniquement, persistance et rechargement.
  - versionning d’artefacts en boucle : `outputs.<node>` retourne la dernière version, `outputs.<node>.history` retourne toutes les versions.
  - reloop global : les `outputs` sont réinitialisés, les `artifacts` persistants survivent.
  - publication explicite d’un artefact persistant et relecture via `artifacts.<name>`.
- Erreurs et retry
  - retry LLM automatique sur rate limit / timeout (3 tentatives, backoff exponentiel), puis échec du nœud.
  - erreur tool renvoyée au LLM dans la tool loop, le LLM adapte sa stratégie.
  - échec de schéma structuré : le nœud échoue sans réparation implicite.
- Intégration goai
  - texte simple, structuré via `WithExplicitSchema`, tool loop avec `WithMaxSteps`, hooks transformés en events.
- Recettes
  - exécution d’une recette avec presets, comparaison de deux recettes, collecte de métriques coût/latence/itérations/verdict final.
- Nœuds tool (sans LLM)
  - nœud tool exécutant un outil directement, produisant un artefact, sans appel LLM.
  - erreur tool propagée comme échec du nœud.
- Outils
  - built-ins, refus de commande non allowlistée, MCP namespacé, erreurs d’outil persistées en events.
- End-to-end
  - `pr_refine_single_model`
  - `pr_refine_dual_model_parallel`
  - `examples/pr_refine_dual_model_parallel_compliance.iter`
  - `ci_fix_until_green`
  - rebouclage global après compliance non atteinte

## Hypothèses et lignes rouges
- Nom du document : `plans/plan-iterion-v3-platform-ready.md`.
- Tous les objectifs décrits ci-dessus font partie de la V1 cible, même si leur livraison sera échelonnée.
- La V1 n’est pas “minimaliste” sur les capacités ; elle est ambitieuse, mais bornée par des choix de runtime stricts et auditables.
- Les outputs structurés du DSL sont stockés en `json.RawMessage` et manipulés en `map[string]any`, pas en types Go générés.
- Le parallélisme V1 est contrôlé et explicite ; pas de parallélisme implicite, non borné ou distribué. Budget partagé first-come-first-served.
- `session: inherit` transmet l'historique du parent direct uniquement. Interdit après un `join`.
- Le `router` est strictement déterministe en V1 ; le routage LLM-based passe par un nœud `agent` + edges conditionnelles.
- Les conditions d’edges V1 restent volontairement simples : booléens explicites sur le output source, pas de langage d’expression.
- Les artefacts sont versionnés en boucle ; `outputs.<node>.history` donne accès à l'historique. Les reloops globaux réinitialisent les `outputs` sauf les artefacts marqués `persistent`.
- Les erreurs LLM transitoires sont retryées automatiquement (3 tentatives). Les erreurs tool sont renvoyées au LLM dans sa tool loop.
- Les checkpoints humains font partie du modèle natif de la V1 ; ils ne doivent pas être implémentés comme un hack externe au runtime.
- Les patterns `plan_then_act`, `review_loop`, `critic_loop`, `pr_refine_dual_model_parallel` vivent comme workflows et recettes de référence, pas comme sucre syntaxique spécial.
- La mutation du workspace reste séquentielle par `WorkingDir` en V1 ; le benchmark multi-recettes impose des workspaces isolés.
- Les budgets monétaires V1 sont exprimés en `max_cost_usd` pour éviter toute ambiguïté d’unité.
- La notion de “qualité” d’une recette n’existe pas par défaut dans le runtime ; elle doit être définie explicitement dans `EvaluationPolicy`.
- Le futur service doit pouvoir réutiliser sans rupture : IR, RunStore, event model, RecipeSpec, ToolRegistry et NodeExecutors.

## Décisions produit validées
- La V1 reste une “plateforme complète ambitieuse”, y compris checkpoints humains natifs et benchmark de recettes.
- La comparaison de recettes en V1 repose sur le verdict final et des métriques objectives minimales : coût, durée et nombre d’itérations.
- Le store V1 est uniquement file-backed ; la compatibilité future service reste un objectif d’architecture, pas un livrable V1.
- La règle “une seule branche mutante par workspace” est validée comme contrainte produit V1, afin de garder un runtime déterministe et sûr.
