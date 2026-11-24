package toolindex

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25 over a node's tools: what `call.search` ranks with.
//
// A tool's document is its name, weighted, its description, and the names and
// descriptions of its top-level parameters. The weighting is by repetition,
// which is what BM25's term frequency reads, so no second scoring rule exists
// beside the formula.

const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// nameWeight is how many times a tool's name counts: a query word that
	// names the tool says more than one that appears in its prose.
	nameWeight = 3
)

type bm25 struct {
	docs   []map[string]int
	lens   []float64
	avgLen float64
	df     map[string]int
}

func newBM25(docs [][]string) *bm25 {
	m := &bm25{df: map[string]int{}}
	total := 0.0
	for _, d := range docs {
		tf := map[string]int{}
		for _, t := range d {
			tf[t]++
		}
		for t := range tf {
			m.df[t]++
		}
		m.docs = append(m.docs, tf)
		m.lens = append(m.lens, float64(len(d)))
		total += float64(len(d))
	}
	if len(docs) > 0 {
		m.avgLen = total / float64(len(docs))
	}
	return m
}

// rank returns the indices of the documents that match at least one query
// term, best first; equal scores keep index order, which is name order.
func (m *bm25) rank(query []string) []int {
	if len(query) == 0 || len(m.docs) == 0 {
		return nil
	}
	terms := map[string]bool{}
	for _, t := range query {
		terms[t] = true
	}
	n := float64(len(m.docs))
	type scored struct {
		i     int
		score float64
	}
	var hits []scored
	for i, tf := range m.docs {
		score := 0.0
		for t := range terms {
			f := float64(tf[t])
			if f == 0 {
				continue
			}
			df := float64(m.df[t])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			norm := 1.0
			if m.avgLen > 0 {
				norm = 1 - bm25B + bm25B*m.lens[i]/m.avgLen
			}
			score += idf * f * (bm25K1 + 1) / (f + bm25K1*norm)
		}
		if score > 0 {
			hits = append(hits, scored{i, score})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	out := make([]int, len(hits))
	for k, h := range hits {
		out[k] = h.i
	}
	return out
}

// documentTokens is a tool's BM25 document.
func documentTokens(t Tool) []string {
	var doc []string
	name := tokens(t.Name)
	for i := 0; i < nameWeight; i++ {
		doc = append(doc, name...)
	}
	doc = append(doc, tokens(t.Description)...)
	if len(t.InputSchema) > 0 {
		if root, err := parseOrdered(t.InputSchema); err == nil {
			if props := root.get("properties"); props != nil && props.kind == kindObject {
				for _, p := range props.obj {
					doc = append(doc, tokens(p.key)...)
					if d := p.val.get("description"); d != nil && d.kind == kindString {
						doc = append(doc, tokens(d.str)...)
					}
				}
			}
		}
	}
	return doc
}

// tokens splits text into lower-case terms: on anything that is not a letter
// or a digit, and inside a word at a lower-to-upper case change, so
// `readFile`, `read_file` and `read-file` all hold "read" and "file".
func tokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && unicode.IsLower(prev) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}
