package operatormcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/repograph"
)

// localMapTools exposes the repository graph (pkg/repograph) to an agent
// working on the checkout the server was started in.
//
// The set is deliberately four tools, not ten. Every tool's schema rides
// in every request before any work is done, which is the cost that makes
// an MCP surface lose to a grep on a small repository; the graph pays for
// itself by answering questions grep cannot — what references this seam,
// what breaks if it changes, is there a path from here to there — not by
// wrapping everything the CLI can do.
func localMapTools() []Tool {
	return []Tool{
		{
			Name: "local_map_find",
			Description: "Find nodes in the repository graph by name: Go packages, exported symbols, docs pages, bots, skills and .bot workflow nodes. " +
				"Returns each node's id (the handle the other local_map_* tools take), its kind, and the file and line to open next. " +
				"Cheaper than a grep when you know part of a name and want the declaration rather than every mention of it. " +
				"The graph is built from the working tree on first use and rebuilt whenever the tree changes.",
			ReadOnly: true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Substring matched against node ids and labels, case-insensitively. An exact label match ranks first."},
    "limit": {"type": "integer", "description": "Maximum nodes to return (default 20)."}
  },
  "required": ["query"],
  "additionalProperties": false
}`),
			handler: handleLocalMapFind,
		},
		{
			Name: "local_map_neighbours",
			Description: "Everything one edge away from a graph node, in both directions: what it imports, contains, declares, calls, references, links to, flows to, or uses — and what does those things to it. " +
				"Relations: imports (package→package), contains (package→file, bot→node), declares (file→symbol), calls (symbol→symbol in call position), references (symbol named without being called — on an interface this is the only edge there is), links (doc→doc), flows (the compiled .bot DAG), uses (bot→skill).",
			ReadOnly: true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":    {"type": "string", "description": "Node id, e.g. pkg:pkg/knowledge, sym:pkg/knowledge.MemoryStore, doc:docs/dsl.md, bot:review-pr, node:review-pr/main.bot#triage. Use local_map_find to get one."},
    "limit": {"type": "integer", "description": "Maximum neighbours to return (default 50); the reply says how many were withheld."}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
			handler: handleLocalMapNeighbours,
		},
		{
			Name: "local_map_impact",
			Description: "What reaches a node — who breaks if it changes. Walks the graph BACKWARDS from the node, bounded by depth. " +
				"On an interface this answers 'which packages and symbols hold this seam', which a call graph alone cannot: a seam is referenced, never called. " +
				"The bound is required rather than a convenience — the unbounded closure of a common symbol is most of the repository.",
			ReadOnly: true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":    {"type": "string", "description": "Node id to walk back from."},
    "depth": {"type": "integer", "description": "How many edges back to walk (default 2)."},
    "limit": {"type": "integer", "description": "Maximum nodes to return (default 50), nearest first."}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
			handler: handleLocalMapImpact,
		},
		{
			Name: "local_map_path",
			Description: "Shortest DIRECTED path between two graph nodes, or nothing when there is none in that direction. " +
				"Answers 'how does this package end up touching that one' with the chain of edges rather than a guess. " +
				"Direction matters: a path from A to B does not imply one from B to A, and the absence of one is an answer.",
			ReadOnly: true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "from": {"type": "string", "description": "Source node id."},
    "to":   {"type": "string", "description": "Target node id."}
  },
  "required": ["from", "to"],
  "additionalProperties": false
}`),
			handler: handleLocalMapPath,
		},
	}
}

// loadGraph builds or reuses the graph for the server's working tree.
//
// In read-only mode it BUILDS without caching. `repograph.Load` writes
// <WorkDir>/.iterion/map/graph.json — 5.9 MB on this repository — and a
// tool annotated ReadOnly that creates files is an annotation the caller
// cannot trust. store() and board() already refuse for the same reason;
// this is the third site of that rule.
func loadGraph(s *Server) (*repograph.Graph, error) {
	if s.ReadOnly {
		g, err := repograph.Build(s.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("build the repository graph: %w", err)
		}
		return g, nil
	}
	g, _, err := repograph.Load(s.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("build the repository graph: %w", err)
	}
	return g, nil
}

// renderNodes is the single place a node list becomes a tool result.
// Truncation is ALWAYS disclosed and an empty result always says which
// of the two emptinesses it is — an unknown id, or a real absence. Three
// handlers used to answer this question three ways, and the one that
// answered `[]` was `impact`, where "nothing" reads as "nothing breaks".
func renderNodes(g *repograph.Graph, id string, nodes []repograph.Node, limit int, empty string) (string, bool, error) {
	if len(nodes) == 0 {
		if id != "" {
			if _, ok := g.Nodes[id]; !ok {
				return fmt.Sprintf("[]\n(no node %q in this graph — use local_map_find to get an id)", id), false, nil
			}
		}
		return "[]\n(" + empty + ")", false, nil
	}
	total := len(nodes)
	if limit > 0 && total > limit {
		nodes = nodes[:limit]
	}
	out, isErr, err := marshalIndent(nodes)
	if err != nil || isErr {
		return out, isErr, err
	}
	if total > len(nodes) {
		out += fmt.Sprintf("\n(showing %d of %d — raise limit for the rest)", len(nodes), total)
	}
	return out, false, nil
}

func handleLocalMapFind(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.Query == "" {
		return "", false, fmt.Errorf("query is required")
	}
	if args.Limit <= 0 {
		args.Limit = 20
	}
	g, err := loadGraph(s)
	if err != nil {
		return "", false, err
	}
	// Unlimited from the query, trimmed here, so the total is known and
	// can be said: "Store" matches 864 nodes and 20 were presented as
	// the answer.
	hits := g.Find(args.Query, 0)
	return renderNodes(g, "", hits, args.Limit,
		fmt.Sprintf("no node matches %q", args.Query))
}

func handleLocalMapNeighbours(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		ID    string `json:"id"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.ID == "" {
		return "", false, fmt.Errorf("id is required")
	}
	g, err := loadGraph(s)
	if err != nil {
		return "", false, err
	}
	if args.Limit <= 0 {
		args.Limit = 50
	}
	ns := g.Neighbours(args.ID)
	if len(ns) == 0 {
		if _, ok := g.Nodes[args.ID]; !ok {
			return fmt.Sprintf("[]\n(no node %q in this graph — use local_map_find to get an id)", args.ID), false, nil
		}
		return fmt.Sprintf("[]\n(no edge touches %s)", args.ID), false, nil
	}
	// Unbounded, this answered 229 neighbours — 15 000 tokens of the
	// context the tool set exists to protect.
	total := len(ns)
	if total > args.Limit {
		ns = ns[:args.Limit]
	}
	out, isErr, err := marshalIndent(ns)
	if err != nil || isErr {
		return out, isErr, err
	}
	if total > len(ns) {
		out += fmt.Sprintf("\n(showing %d of %d — raise limit for the rest)", len(ns), total)
	}
	return out, false, nil
}

func handleLocalMapImpact(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		ID    string `json:"id"`
		Depth int    `json:"depth"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.ID == "" {
		return "", false, fmt.Errorf("id is required")
	}
	if args.Limit <= 0 {
		args.Limit = 50
	}
	g, err := loadGraph(s)
	if err != nil {
		return "", false, err
	}
	depth := args.Depth
	if depth <= 0 {
		depth = 2
	}
	nodes := g.Impacted(args.ID, depth)
	return renderNodes(g, args.ID, nodes, args.Limit,
		fmt.Sprintf("nothing reaches %s within depth %d", args.ID, depth))
}

func handleLocalMapPath(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.From == "" || args.To == "" {
		return "", false, fmt.Errorf("from and to are both required")
	}
	g, err := loadGraph(s)
	if err != nil {
		return "", false, err
	}
	for _, id := range []string{args.From, args.To} {
		if _, ok := g.Nodes[id]; !ok {
			return fmt.Sprintf("[]\n(no node %q in this graph — use local_map_find to get an id)", id), false, nil
		}
	}
	hops := g.Path(args.From, args.To)
	if len(hops) == 0 {
		return fmt.Sprintf("[]\n(no directed path from %s to %s)", args.From, args.To), false, nil
	}
	return marshalIndent(hops)
}

func marshalIndent(v any) (string, bool, error) {
	body, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return "", false, fmt.Errorf("encode result: %w", err)
	}
	return string(body), false, nil
}
