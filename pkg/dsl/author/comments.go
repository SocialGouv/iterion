package author

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Comments lists the YAML comments src carries — head, line and foot
// comments of every node, one entry per comment line, in document order.
// The writer keeps none of them (Write renders the program, not the
// document), so a rewrite of a document that carries one would lose it:
// `iterion fmt` asks here before it rewrites a document in its canonical
// form, and refuses when the answer is not empty. A source that is not YAML
// has no comments to report.
func Comments(src []byte) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil
	}
	var out []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		for _, block := range []string{n.HeadComment, n.LineComment, n.FootComment} {
			for _, line := range strings.Split(block, "\n") {
				if strings.TrimSpace(line) != "" {
					out = append(out, strings.TrimSpace(line))
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	return out
}
