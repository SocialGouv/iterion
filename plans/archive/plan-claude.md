# Plan : iterion — CLI Go de boucles LLM coding configurables

## Contexte

On construit **iterion**, un outil CLI en Go qui permet de décrire et d'exécuter des workflows d'agents LLM orientés coding via un DSL custom à flèches. L'outil est pensé pour être portable (pipelines CI/CD) et modulaire (tools pluggables, MCP, multi-providers LLM).

La librairie **goai** (`github.com/zendev-sh/goai`) fournit toute l'interaction LLM : génération texte/structurée, streaming, tool loops, 20+ providers. iterion orchestre les agents par-dessus.

**Problème résolu** : aujourd'hui, orchestrer des boucles LLM coding (plan→code→review→fix) nécessite du code custom à chaque fois. iterion permet de décrire ces workflows dans un DSL déclaratif, de les valider statiquement, et de les exécuter de manière reproductible.

---

## Architecture globale

```
DSL file (.iter) ──→ Lexer/Parser ──→ AST ──→ Compiler ──→ IR ──→ Runtime Engine ──→ Output
                                                                        ↕
                                                                    goai (LLM)
                                                                    Tools (built-in + MCP)
```

Le DSL n'est **pas** la source d'exécution. L'IR canonique l'est.
Mermaid est une **vue dérivée** de l'IR.

---

## Structure du projet

```
github.com/iterion-dev/iterion/
├── main.go
├── cmd/
│   ├── root.go              # cobra root
│   ├── run.go               # iterion run <file>
│   ├── validate.go          # iterion validate <file>
│   └── diagram.go           # iterion diagram <file>
├── dsl/
│   ├── token.go             # types de tokens
│   ├── lexer.go             # lexer indent-aware
│   ├── ast.go               # nœuds AST
│   └── parser.go            # parseur récursif descendant
├── ir/
│   ├── ir.go                # types IR canoniques
│   └── validate.go          # règles de validation
├── compiler/
│   ├── compiler.go          # AST → IR + résolution refs
│   └── resolve.go           # interpolation env vars, résolution modèles
├── runtime/
│   ├── engine.go            # moteur d'exécution séquentiel
│   ├── state.go             # RunState, vars, compteurs boucles, trace
│   └── condition.go         # évaluateur de conditions
├── tool/
│   ├── tool.go              # Registry + interface
│   ├── builtin.go           # enregistrement des tools V1
│   ├── read_file.go         # read_file
│   ├── write_file.go        # write_file
│   ├── list_files.go        # list_files
│   ├── run_command.go       # run_command (shell)
│   ├── git.go               # git_diff, git_status, git_commit
│   ├── search.go            # search_codebase (grep/glob)
│   ├── patch.go             # édition/patch de fichiers
│   └── tree.go              # arborescence répertoire
├── mcp/
│   ├── client.go            # client MCP, découverte tools
│   └── transport.go         # transports stdio / SSE
├── llm/
│   ├── adapter.go           # pont runtime ↔ goai
│   └── model_registry.go    # parse "provider/model" → LanguageModel
├── mermaid/
│   └── generate.go          # IR → diagramme Mermaid
└── output/
    ├── formatter.go          # JSON vs human-friendly
    └── logger.go             # logging structuré (slog)
```

**Dépendances** : `github.com/zendev-sh/goai`, `github.com/spf13/cobra`, stdlib (`log/slog`, `encoding/json`, etc.)

---

## DSL — Syntaxe concrète

### Principes
- Indentation-sensitive (2 espaces)
- `${VAR}` → interpolé à la compilation depuis les env vars
- `{{var}}` → interpolé au runtime depuis l'état du workflow
- Mots-clés réservés : `workflow`, `vars`, `prompt`, `schema`, `agent`, `when`, `with`, `as`, `done`, `fail`

### Exemple complet

```
vars:
  project_dir: string = "${PROJECT_DIR}"
  language: string = "go"

prompt review_system:
  Tu es un reviewer senior. Review le code pour la correction,
  le style et les bugs potentiels. Produis un avis structuré.

prompt coder_system:
  Tu es un développeur {{language}} expert. Implémente les changements
  demandés en suivant le feedback du reviewer.

schema review_result:
  approved: bool
  feedback: string
  issues: string [enum: "none", "minor", "major"]

agent planner:
  model: anthropic/claude-sonnet-4-20250514
  system: "Tu es un agent de planification. Décompose la tâche en étapes."
  tools: [read_file, list_files, search_codebase, tree]

agent coder:
  model: anthropic/claude-sonnet-4-20250514
  system: coder_system
  tools: [read_file, write_file, run_command, patch, git_diff]

agent reviewer:
  model: anthropic/claude-sonnet-4-20250514
  system: review_system
  tools: [read_file, search_codebase, git_diff]
  output: review_result

workflow code_review:
  vars: { task: string }

  planner -> coder
  coder -> reviewer
  reviewer -> coder when not_approved as review_loop(3)
  reviewer -> done when approved
```

### Grammaire (EBNF simplifié)

```ebnf
File         = { Section } .
Section      = WorkflowDecl | VarsDecl | PromptDecl | SchemaDecl | AgentDecl .

WorkflowDecl = "workflow" IDENT ":" NEWLINE INDENT { VarsLine | EdgeLine } DEDENT .
EdgeLine     = IDENT "->" IDENT [ "when" IDENT ] [ "with" "{" KVList "}" ] [ "as" IDENT "(" INT ")" ] NEWLINE .

AgentDecl    = "agent" IDENT ":" NEWLINE INDENT { AgentProp } DEDENT .
AgentProp    = "model:" STRING | "system:" (STRING | IDENT) | "prompt:" (STRING | IDENT)
             | "tools:" "[" IdentList "]" | "output:" IDENT | "temperature:" FLOAT | "max_tokens:" INT .

SchemaDecl   = "schema" IDENT ":" NEWLINE INDENT { IDENT ":" Type [ "[" "enum:" StringList "]" ] } DEDENT .
PromptDecl   = "prompt" IDENT ":" NEWLINE INDENT TextBlock DEDENT .
VarsDecl     = "vars:" NEWLINE INDENT { IDENT ":" Type [ "=" DefaultValue ] } DEDENT .
```

---

## Types AST (dsl/)

```go
type File struct {
    Workflows []WorkflowDecl
    Vars      []VarDecl
    Prompts   []PromptDecl
    Schemas   []SchemaDecl
    Agents    []AgentDecl
}

type EdgeDecl struct {
    Pos      Pos
    From     string
    To       string            // "done" et "fail" réservés
    When     string            // nom de condition, vide = inconditionnel
    With     map[string]string
    LoopName string
    LoopMax  int               // > 0 obligatoire si LoopName != ""
}

type AgentDecl struct {
    Pos         Pos
    Name        string
    Model       string   // "provider/model-id"
    System      string   // string littérale ou ref prompt
    Prompt      string
    Tools       []string
    Output      string   // ref schema
    Temperature *float64
    MaxTokens   *int
}
```

---

## Types IR (ir/)

L'IR est la forme validée, résolue, prête à l'exécution. Sérialisable en JSON.

```go
type Workflow struct {
    Name      string            `json:"name"`
    Vars      []VarDef          `json:"vars"`
    Nodes     map[string]*Node  `json:"nodes"`
    Edges     []*Edge           `json:"edges"`
    EntryNode string            `json:"entry_node"`
}

type Node struct {
    Name         string          `json:"name"`
    Type         NodeType        `json:"type"`         // "agent" | "terminal"
    Model        string          `json:"model,omitempty"`
    System       string          `json:"system,omitempty"`
    Prompt       string          `json:"prompt,omitempty"`
    Tools        []string        `json:"tools,omitempty"`
    OutputSchema json.RawMessage `json:"output_schema,omitempty"` // JSON Schema compilé
    Temperature  *float64        `json:"temperature,omitempty"`
    MaxTokens    *int            `json:"max_tokens,omitempty"`
}

type Edge struct {
    From      string            `json:"from"`
    To        string            `json:"to"`
    Condition string            `json:"condition,omitempty"`
    With      map[string]string `json:"with,omitempty"`
    LoopName  string            `json:"loop_name,omitempty"`
    LoopMax   int               `json:"loop_max,omitempty"`
}
```

### Règles de validation IR
1. Chaque agent référencé dans les edges est déclaré
2. Chaque `From`/`To` résolu vers un agent ou terminal (`done`/`fail`)
3. Chaque nœud non-terminal a au moins une arête sortante
4. Un seul nœud d'entrée (premier `From` de la première edge)
5. Toutes les boucles ont `LoopMax > 0`
6. Les conditions `when` correspondent à des champs du schema de sortie du nœud source
7. Les placeholders `{{var}}` dans les prompts résolvent vers des vars déclarées ou des outputs de nœuds
8. Pas de nœuds inatteignables
9. Au moins un chemin atteint un terminal

---

## Moteur d'exécution (runtime/)

### RunState

```go
type RunState struct {
    RunID       string
    Status      RunStatus                  // running | completed | failed
    Vars        map[string]any
    NodeOutputs map[string]json.RawMessage // node_name → dernier output
    LoopCounts  map[string]int             // loop_name → compteur courant
    Trace       []TraceEntry
    StartedAt   time.Time
    Error       error
}

type TraceEntry struct {
    Timestamp time.Time       `json:"ts"`
    Node      string          `json:"node"`
    Type      string          `json:"type"` // enter, exit, tool_call, edge, error
    Data      json.RawMessage `json:"data,omitempty"`
    Duration  time.Duration   `json:"duration,omitempty"`
    Usage     *TokenUsage     `json:"usage,omitempty"`
}
```

### Algorithme d'exécution (séquentiel V1)

```
currentNode = workflow.EntryNode

boucle:
  node = workflow.Nodes[currentNode]

  si node.Type == "terminal":
    si "done" → status = completed
    si "fail" → status = failed
    retourner résultat

  output, usage = executeNode(ctx, node, state)  // appel goai
  state.NodeOutputs[node.Name] = output

  edges = outgoingEdges(node.Name)
  nextEdge = evaluateEdges(edges, output, state)

  si nextEdge.LoopName != "":
    state.LoopCounts[nextEdge.LoopName]++
    si count > nextEdge.LoopMax:
      nextEdge = fallbackEdge(edges, loopName)  // edge sans loop
      si nil → erreur "boucle épuisée sans fallback"

  currentNode = nextEdge.To
  goto boucle
```

### Évaluation des conditions (V1)

Simples vérifications booléennes sur les champs du output structuré :
- `when approved` → `output.approved == true`
- `when not_approved` → `output.approved != true`
- vide → toujours vrai (edge par défaut)

---

## Adaptateur LLM (llm/)

### Model Registry

Parse `"provider/model-id"` → `provider.LanguageModel` via goai :

```go
func ResolveModel(spec string) (provider.LanguageModel, error) {
    // "anthropic/claude-sonnet-4-20250514" → anthropic.Chat("claude-sonnet-4-20250514")
    // "openai/gpt-4o" → openai.Chat("gpt-4o")
    // etc. pour les 20+ providers goai
}
```

Override via env : `ITERION_<AGENT_NAME>_MODEL` ou `ITERION_DEFAULT_MODEL`.

### Adapter

```go
type Adapter interface {
    Execute(ctx context.Context, req *ExecuteRequest) (*ExecuteResult, error)
}

type ExecuteRequest struct {
    Node        *ir.Node
    Messages    []provider.Message
    RuntimeVars map[string]any      // pour interpolation prompts
    Tools       []goai.Tool
}

type ExecuteResult struct {
    Output json.RawMessage
    Text   string
    Usage  TokenUsage
}
```

Logique :
- Si `node.OutputSchema != nil` → `goai.GenerateObject` avec le schema
- Sinon → `goai.GenerateText`
- Tools injectés via `goai.WithTools()`
- Boucle d'outils déléguée à goai via `goai.WithMaxSteps(10)`

---

## Système de tools (tool/)

### Registry

```go
type Registry struct { tools map[string]goai.Tool }

func (r *Registry) Register(t goai.Tool)
func (r *Registry) Resolve(names []string) ([]goai.Tool, error)
```

### Tools built-in V1

| Tool | Description | Champs input |
|------|-------------|--------------|
| `read_file` | Lire un fichier | `path`, `offset?`, `limit?` |
| `write_file` | Écrire un fichier | `path`, `content` |
| `list_files` | Lister fichiers | `path`, `pattern?` |
| `run_command` | Exécuter commande shell | `command`, `timeout_ms?` |
| `git_diff` | Voir les diffs | `ref?` |
| `git_status` | Status git | — |
| `git_commit` | Committer | `message`, `files?` |
| `search_codebase` | Grep/glob | `pattern`, `glob?`, `path?` |
| `patch` | Éditer un fichier | `path`, `old_string`, `new_string` |
| `tree` | Arborescence | `path?`, `depth?` |

Chaque tool est un `goai.Tool` avec une fonction `Execute` concrète opérant dans le working directory configuré.

### MCP (mcp/)

```go
type Client struct { transport Transport }

type Transport interface {
    Initialize(ctx context.Context) error
    ListTools(ctx context.Context) ([]ToolInfo, error)
    CallTool(ctx context.Context, name string, input []byte) (string, error)
    Close() error
}

func (c *Client) AsGoAITools(ctx context.Context) ([]goai.Tool, error)
```

Transports : **stdio** (spawn subprocess) et **SSE** (HTTP endpoint).
Configuration V1 via flags CLI : `--mcp "npx -y @anthropic-ai/claude-code --mcp"`.

---

## Mermaid (mermaid/)

Mapping IR → `flowchart TD` :
- Agent → rectangle arrondi : `node["node"]`
- Terminal done → stade : `done(["done"])`
- Terminal fail → stade rouge : `fail(["fail"])`
- Edge inconditionnelle → `A --> B`
- Edge conditionnelle → `A -->|"condition"| B`
- Edge loop → ligne pointillée : `A -.->|"not_approved (max 3)"| B`

---

## CLI (cmd/)

```
iterion run <file.iter> [flags]
  --workflow <name>     # si plusieurs workflows dans le fichier
  --var key=value       # variables passées au workflow
  --mcp <command>       # serveur MCP à connecter (répétable)
  --json                # sortie JSON (défaut si non-TTY)
  --verbose             # logs détaillés
  --dry-run             # compile et valide sans exécuter

iterion validate <file.iter>
  # Parse, compile, valide. Affiche diagnostics. Exit 0 si valide.

iterion diagram <file.iter> [--workflow <name>] [--output <file>]
  # Génère diagramme Mermaid depuis l'IR.
```

**Codes de sortie** : 0 = `done`, 1 = `fail`, 2 = erreur runtime, 3 = erreur parse/compile

**Détection TTY** : `os.Stdout` est un terminal → format humain coloré. Sinon ou `--json` → JSON lines.

---

## Phases d'implémentation

### Phase 1 : Fondations — Lexer/Parser + types IR
**Objectif** : Parser des fichiers DSL en AST.
**Scope** : `dsl/` (token, lexer, parser, ast), `ir/ir.go` (types seulement)
**Fichiers** : `dsl/token.go`, `dsl/lexer.go`, `dsl/ast.go`, `dsl/parser.go`, `ir/ir.go`
**Critère** : `dsl.ParseFile("example.iter")` retourne un `*dsl.File` correct. Tests exhaustifs.

### Phase 2 : Compilateur + validation
**Objectif** : Transformer AST en IR validée.
**Scope** : `compiler/`, `ir/validate.go`
**Fichiers** : `compiler/compiler.go`, `compiler/resolve.go`, `ir/validate.go`, `cmd/validate.go`
**Critère** : `compiler.Compile()` produit un `*ir.Workflow` valide. `iterion validate` fonctionne.
**Dépend de** : Phase 1

### Phase 3 : Système de tools (parallélisable avec Phase 1-2)
**Objectif** : Implémenter les 10 tools coding built-in.
**Scope** : `tool/`
**Fichiers** : `tool/tool.go`, `tool/builtin.go`, `tool/read_file.go`, ... `tool/tree.go`
**Critère** : Registry avec 10 tools testés unitairement.

### Phase 4 : Adaptateur LLM + runtime minimal
**Objectif** : Exécuter un workflow linéaire simple (A → B → done).
**Scope** : `llm/`, `runtime/` (engine, state), `cmd/run.go`
**Fichiers** : `llm/adapter.go`, `llm/model_registry.go`, `runtime/engine.go`, `runtime/state.go`, `cmd/run.go`, `cmd/root.go`, `main.go`
**Critère** : `iterion run simple.iter --var task="hello"` exécute un workflow linéaire.
**Dépend de** : Phase 2, Phase 3

### Phase 5 : Conditions + boucles
**Objectif** : Support complet des branchements conditionnels et boucles bornées.
**Scope** : `runtime/condition.go`, mise à jour `runtime/engine.go`
**Critère** : Le workflow `code_review` complet avec `review_loop(3)` fonctionne.
**Dépend de** : Phase 4

### Phase 6 : Mermaid + CLI polish
**Objectif** : Génération de diagrammes, sortie human-friendly, CLI complète.
**Scope** : `mermaid/`, `output/`, `cmd/diagram.go`
**Critère** : `iterion diagram` produit du Mermaid valide. Sortie formatée selon TTY.
**Dépend de** : Phase 2

### Phase 7 : Intégration MCP
**Objectif** : Connecter des serveurs MCP et exposer leurs tools aux agents.
**Scope** : `mcp/`
**Critère** : `--mcp "command"` fonctionne, tools MCP disponibles pour les agents.
**Dépend de** : Phase 4

### Phase 8 : Durcissement
**Objectif** : Gestion d'erreurs, timeouts, exit codes, robustesse pipeline.
**Scope** : Transversal.
**Critère** : Arrêt gracieux (SIGINT), messages d'erreur clairs, codes de sortie corrects.

---

## Décisions de design clés

1. **DSL indent-sensitive** : Oui. Plus propre pour le format visuel à flèches.
2. **Conditions V1** : Vérifications booléennes sur champs seulement (`when approved`, `when not_approved`). Pas de langage d'expressions.
3. **Exécution séquentielle V1** : Pas de branches parallèles. Un nœud à la fois.
4. **Boucle d'outils déléguée à goai** : `WithMaxSteps(10)` par défaut. iterion gère uniquement l'orchestration inter-nœuds.
5. **Résolution modèle à la compilation** : `"provider/model"` parsé et validé. Override env var appliqué à la compilation.
6. **Prompts = templates, résolus au runtime** : `{{var}}` interpolé juste avant chaque exécution de nœud.

---

## Vérification

Pour tester le système end-to-end :
1. Écrire un fichier `examples/simple.iter` avec un workflow linéaire simple
2. `iterion validate examples/simple.iter` → exit 0
3. `iterion diagram examples/simple.iter` → Mermaid valide
4. `iterion run examples/simple.iter --var task="Write a hello world"` → exécution complète
5. `iterion run examples/code_review.iter --var task="Add error handling" --json` → sortie JSON structurée
6. Tests unitaires par package : `go test ./...`

---

## Fichiers goai critiques à réutiliser

- `~/goai/types.go` : struct `goai.Tool` — nos tools doivent produire ce type
- `~/goai/generate.go` : `GenerateText()` et API streaming
- `~/goai/object.go` : `GenerateObject[T]()` pour les sorties structurées
- `~/goai/options.go` : toutes les fonctions `With*()` pour configurer les appels
- `~/goai/provider/provider.go` : interface `LanguageModel`
- `~/goai/provider/types.go` : `Message`, `Part`, `Usage`
- `~/goai/messages.go` : helpers `SystemMessage()`, `UserMessage()`, etc.
- `~/goai/provider/anthropic/` : `anthropic.Chat()` pour résolution provider
- `~/goai/provider/openai/` : `openai.Chat()` pour résolution provider

