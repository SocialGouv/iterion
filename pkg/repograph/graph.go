// Package repograph builds one deterministic graph of this repository:
// Go packages and the symbols they declare, the calls between them, the
// links between documentation pages, and — the part no external tool
// models — the DAG of every `.bot` workflow, its nodes, its edges and the
// skills it grants.
//
// Three properties, and each of them is a decision:
//
//   - DETERMINISTIC. go/parser, markdown links, and the engine's own
//     workflow compiler. No model call, no embedding: the literature puts
//     LLM-extracted indexing at roughly a thousand times the cost of a
//     vector index, and this project's runs go through subscription
//     backends with no API key to spend on an indexing pass. See
//     docs/references/context-retrieval-state-of-the-art.md.
//   - NO DATABASE. A flat artifact plus a traversal in Go. Kuzu was
//     archived in 2025 and RedisGraph reached EOL in 2023; a graph engine
//     that outlives its vendor is worth more here than Cypher.
//   - REBUILT, NOT PATCHED. The cache is keyed on a fingerprint of the
//     tree; any change rebuilds the whole graph. Incremental invalidation
//     would be faster and would introduce exactly the failure this graph
//     exists to avoid — an index that describes a repository that no
//     longer exists.
package repograph

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Kind is what a node stands for.
type Kind string

const (
	KindPackage Kind = "package"
	KindFile    Kind = "file"
	KindSymbol  Kind = "symbol"
	KindDoc     Kind = "doc"
	KindBot     Kind = "bot"
	KindSkill   Kind = "skill"
	KindDSLNode Kind = "dsl-node"
)

// Rel is what an edge means.
type Rel string

const (
	RelImports  Rel = "imports"  // package → package
	RelContains Rel = "contains" // package → file, bot → dsl-node
	RelDeclares Rel = "declares" // file → symbol
	RelCalls    Rel = "calls"    // symbol → symbol, in call position
	// RelReferences is a symbol named without being called: a parameter
	// type, a struct field, a variable's type. On an interface — which is
	// what this project calls a seam — it is the ONLY edge there is, so a
	// graph that recorded calls alone would answer "nothing depends on
	// this" about the most depended-upon declarations in the tree.
	RelReferences Rel = "references"
	RelLinks      Rel = "links" // doc → doc
	RelFlows      Rel = "flows" // dsl-node → dsl-node (the .bot DAG)
	RelUses       Rel = "uses"  // bot → skill
)

// Node is one vertex. ID is stable and greppable by construction:
// "pkg:pkg/knowledge", "sym:pkg/knowledge.MemoryStore",
// "bot:review-pr", "node:review-pr/main.bot#triage".
type Node struct {
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	// Path is the repo-relative file this node comes from, when it has
	// one. It is what a reader opens next.
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
	Doc  string `json:"doc,omitempty"`
}

// Edge is one directed relation.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Rel  Rel    `json:"rel"`
}

// Graph is the whole thing: nodes by id, edges, and the adjacency both
// ways. Out and In are derived at load time, never serialised — a
// persisted adjacency is one more thing that can disagree with the edges.
type Graph struct {
	BuiltAt     string          `json:"built_at"`
	Fingerprint string          `json:"fingerprint"`
	Nodes       map[string]Node `json:"nodes"`
	Edges       []Edge          `json:"edges"`

	out map[string][]Edge
	in  map[string][]Edge
}

// NewGraph returns an empty graph ready to be filled.
func NewGraph() *Graph {
	return &Graph{Nodes: map[string]Node{}}
}

// AddNode inserts a node, keeping the first non-empty Doc and Path seen
// for an id: a symbol declared once and referenced often must not lose
// its definition site to a later mention.
func (g *Graph) AddNode(n Node) {
	if existing, ok := g.Nodes[n.ID]; ok {
		if n.Doc == "" {
			n.Doc = existing.Doc
		}
		if n.Path == "" {
			n.Path = existing.Path
			n.Line = existing.Line
		}
	}
	g.Nodes[n.ID] = n
}

// AddEdge records a relation. Edges to unknown nodes are kept: an import
// of a package outside the module is a fact about the importer, and the
// query layer filters dangling targets rather than the builder dropping
// them silently.
func (g *Graph) AddEdge(from, to string, rel Rel) {
	if from == "" || to == "" || from == to {
		return
	}
	g.Edges = append(g.Edges, Edge{From: from, To: to, Rel: rel})
}

// Finalise sorts and de-duplicates so two builds of the same tree produce
// identical bytes, then indexes the adjacency.
func (g *Graph) Finalise() {
	sort.Slice(g.Edges, func(i, j int) bool {
		a, b := g.Edges[i], g.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Rel < b.Rel
	})
	deduped := g.Edges[:0]
	var prev Edge
	for i, e := range g.Edges {
		if i > 0 && e == prev {
			continue
		}
		deduped = append(deduped, e)
		prev = e
	}
	g.Edges = deduped
	g.index()
}

func (g *Graph) index() {
	g.out = make(map[string][]Edge, len(g.Nodes))
	g.in = make(map[string][]Edge, len(g.Nodes))
	for _, e := range g.Edges {
		g.out[e.From] = append(g.out[e.From], e)
		g.in[e.To] = append(g.in[e.To], e)
	}
}

// Out returns the edges leaving a node; In, those arriving.
func (g *Graph) Out(id string) []Edge {
	if g.out == nil {
		g.index()
	}
	return g.out[id]
}

func (g *Graph) In(id string) []Edge {
	if g.in == nil {
		g.index()
	}
	return g.in[id]
}

// Write serialises the graph. Encoding is deterministic: Nodes is a map,
// so the encoder sorts its keys, and Edges was sorted by Finalise.
func (g *Graph) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	return enc.Encode(g)
}

// Read loads a graph and rebuilds its adjacency.
func Read(r io.Reader) (*Graph, error) {
	var g Graph
	if err := json.NewDecoder(r).Decode(&g); err != nil {
		return nil, fmt.Errorf("repograph: decode: %w", err)
	}
	if g.Nodes == nil {
		g.Nodes = map[string]Node{}
	}
	g.index()
	return &g, nil
}

// Stats counts nodes per kind and edges per relation — the first thing
// anyone asks of a graph, and the cheapest way to see a build go wrong.
func (g *Graph) Stats() (map[Kind]int, map[Rel]int) {
	nodes := map[Kind]int{}
	for _, n := range g.Nodes {
		nodes[n.Kind]++
	}
	edges := map[Rel]int{}
	for _, e := range g.Edges {
		edges[e.Rel]++
	}
	return nodes, edges
}

// symbolID and the other id helpers are the single place each id shape is
// written. A second spelling anywhere else is how a graph grows two nodes
// for one thing.
func packageID(dir string) string { return "pkg:" + dir }
func fileID(path string) string   { return "file:" + path }
func symbolID(dir, name string) string {
	return "sym:" + dir + "." + name
}
func docID(path string) string { return "doc:" + path }
func botID(name string) string { return "bot:" + name }
func skillID(bot, file string) string {
	return "skill:" + bot + "/" + file
}
func dslNodeID(bot, workflow, node string) string {
	return "node:" + bot + "/" + workflow + "#" + node
}
