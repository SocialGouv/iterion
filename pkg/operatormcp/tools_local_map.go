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
				"Relations: imports (package→package), contains (package→file, bot→node), declares (file→symbol), calls (symbol→symbol in call position), references (symbol named without being called — on an interface this is the only edge there is), links (doc→doc), flows (the compiled .bot DAG), uses (bot→skill). " +
				"An edge whose target is outside the graph is returned with missing:true rather than dropped.",
			ReadOnly: true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "Node id, e.g. pkg:pkg/knowledge, sym:pkg/knowledge.MemoryStore, doc:docs/dsl.md, bot:review-pr, node:review-pr/main.bot#triage. Use local_map_find to get one."}
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
func loadGraph(s *Server) (*repograph.Graph, error) {
	g, _, err := repograph.Load(s.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("build the repository graph: %w", err)
	}
	return g, nil
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
	hits := g.Find(args.Query, args.Limit)
	if len(hits) == 0 {
		return fmt.Sprintf("[]\n(no node matches %q)", args.Query), false, nil
	}
	return marshalIndent(hits)
}

func handleLocalMapNeighbours(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		ID string `json:"id"`
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
	ns := g.Neighbours(args.ID)
	if len(ns) == 0 {
		return fmt.Sprintf("[]\n(no edge touches %q — use local_map_find to get a node id)", args.ID), false, nil
	}
	return marshalIndent(ns)
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
	nodes := g.Impacted(args.ID, args.Depth)
	truncated := len(nodes) > args.Limit
	if truncated {
		nodes = nodes[:args.Limit]
	}
	out, isErr, err := marshalIndent(nodes)
	if err != nil || isErr {
		return out, isErr, err
	}
	if truncated {
		// Saying so is the point: a silently truncated impact list reads
		// as "that is all of it", which is the answer nobody should act on.
		out += fmt.Sprintf("\n(truncated to %d; raise limit or lower depth for the rest)", args.Limit)
	}
	return out, false, nil
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
