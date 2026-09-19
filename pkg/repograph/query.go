package repograph

import (
	"sort"
	"strings"
)

// Find returns the nodes whose id or label contains the query, ranked so
// an exact label match comes first. Matching is case-insensitive.
func (g *Graph) Find(query string, limit int) []Node {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var hits []Node
	for _, n := range g.Nodes {
		id, label := strings.ToLower(n.ID), strings.ToLower(n.Label)
		if strings.Contains(id, q) || strings.Contains(label, q) {
			hits = append(hits, n)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		si, sj := matchScore(hits[i], q), matchScore(hits[j], q)
		if si != sj {
			return si > sj
		}
		return hits[i].ID < hits[j].ID
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func matchScore(n Node, q string) int {
	label := strings.ToLower(n.Label)
	switch {
	case label == q:
		return 3
	case strings.HasPrefix(label, q):
		return 2
	case strings.Contains(label, q):
		return 1
	default:
		return 0
	}
}

// Neighbour is one step away from a node, with the relation and the
// direction that got there.
type Neighbour struct {
	Node     Node   `json:"node"`
	Rel      Rel    `json:"rel"`
	Incoming bool   `json:"incoming,omitempty"`
	Missing  bool   `json:"missing,omitempty"` // an edge to something outside the graph
	ID       string `json:"id"`
}

// Neighbours returns everything one edge away, outgoing first. An edge
// whose target is not a node of this graph is returned with Missing set
// rather than dropped: "this package imports something we do not index"
// is an answer, and hiding it would make the graph look more complete
// than it is.
func (g *Graph) Neighbours(id string) []Neighbour {
	var out []Neighbour
	for _, e := range g.Out(id) {
		n, ok := g.Nodes[e.To]
		out = append(out, Neighbour{Node: n, Rel: e.Rel, ID: e.To, Missing: !ok})
	}
	for _, e := range g.In(id) {
		n, ok := g.Nodes[e.From]
		out = append(out, Neighbour{Node: n, Rel: e.Rel, ID: e.From, Incoming: true, Missing: !ok})
	}
	return out
}

// Path returns the shortest directed path from → to, as node ids, or nil
// when none exists. Breadth-first, so the first path found is a shortest
// one; ties are broken by sorted edge order, so the answer is the same
// on every run of the same graph.
func (g *Graph) Path(from, to string) []string {
	if from == to {
		if _, ok := g.Nodes[from]; ok {
			return []string{from}
		}
		return nil
	}
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.Out(cur) {
			if _, seen := prev[e.To]; seen {
				continue
			}
			prev[e.To] = cur
			if e.To == to {
				return rebuildPath(prev, from, to)
			}
			queue = append(queue, e.To)
		}
	}
	return nil
}

func rebuildPath(prev map[string]string, from, to string) []string {
	var reversed []string
	for at := to; at != ""; at = prev[at] {
		reversed = append(reversed, at)
		if at == from {
			break
		}
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

// Impacted returns everything that can reach the given node within
// maxDepth steps — who breaks if this changes. Depth 0 means the node
// itself; the result excludes it.
//
// The bound is required, not a convenience: on a call graph the
// transitive closure of a widely-used symbol is most of the repository,
// and "everything" is not an answer anyone can act on.
func (g *Graph) Impacted(id string, maxDepth int) []Node {
	if maxDepth <= 0 {
		maxDepth = 2
	}
	seen := map[string]int{id: 0}
	frontier := []string{id}
	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, cur := range frontier {
			for _, e := range g.In(cur) {
				if _, ok := seen[e.From]; ok {
					continue
				}
				seen[e.From] = depth
				next = append(next, e.From)
			}
		}
		frontier = next
	}
	delete(seen, id)

	out := make([]Node, 0, len(seen))
	for nodeID := range seen {
		if n, ok := g.Nodes[nodeID]; ok {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := seen[out[i].ID], seen[out[j].ID]
		if di != dj {
			return di < dj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Rank scores nodes by personalised PageRank seeded on the given ids,
// which is how a repo map decides what is worth showing about a place
// you are already standing in. With no seeds it degenerates to plain
// PageRank — the repository's own centre of gravity.
//
// Deterministic: fixed iteration count, sorted tie-breaks, no map
// iteration in the arithmetic.
func (g *Graph) Rank(seeds []string, limit int) []RankedNode {
	const (
		damping    = 0.85
		iterations = 20
	)
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		index[id] = i
	}

	restart := make([]float64, len(ids))
	seedSet := map[string]bool{}
	for _, s := range seeds {
		if i, ok := index[s]; ok {
			restart[i] = 1
			seedSet[s] = true
		}
	}
	total := 0.0
	for _, v := range restart {
		total += v
	}
	if total == 0 {
		for i := range restart {
			restart[i] = 1 / float64(len(ids))
		}
	} else {
		for i := range restart {
			restart[i] /= total
		}
	}

	outDegree := make([]int, len(ids))
	for _, e := range g.Edges {
		if i, ok := index[e.From]; ok {
			if _, known := index[e.To]; known {
				outDegree[i]++
			}
		}
	}

	score := make([]float64, len(ids))
	copy(score, restart)
	next := make([]float64, len(ids))
	for iter := 0; iter < iterations; iter++ {
		for i := range next {
			next[i] = (1 - damping) * restart[i]
		}
		dangling := 0.0
		for i := range ids {
			if outDegree[i] == 0 {
				dangling += score[i]
			}
		}
		for _, e := range g.Edges {
			i, okFrom := index[e.From]
			j, okTo := index[e.To]
			if !okFrom || !okTo || outDegree[i] == 0 {
				continue
			}
			next[j] += damping * score[i] / float64(outDegree[i])
		}
		// A node with no outgoing edge would otherwise leak its mass out
		// of the system; give it back along the restart vector.
		for i := range next {
			next[i] += damping * dangling * restart[i]
		}
		score, next = next, score
	}

	ranked := make([]RankedNode, 0, len(ids))
	for i, id := range ids {
		if seedSet[id] {
			continue // the seeds are where you already are
		}
		n, ok := g.Nodes[id]
		if !ok {
			continue
		}
		ranked = append(ranked, RankedNode{Node: n, Score: score[i]})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Node.ID < ranked[j].Node.ID
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

// RankedNode is a node with its personalised-PageRank score.
type RankedNode struct {
	Node  Node    `json:"node"`
	Score float64 `json:"score"`
}
