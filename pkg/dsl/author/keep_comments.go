package author

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// keepComments carries the comments of src — a document as yaml.v3 read
// it — onto dst, the tree the writer built for the program src describes.
// A comment rides the node it hangs on: dst's node at the same path takes
// its head, line and foot comments. The path is read through the document
// root, a mapping's entries by key, and a sequence's items by what they say
// (itemSignature) — never by position alone, so a comment cannot land on a
// neighbour the writer put in its place. A node dst does not hold at the
// path keeps nothing, and its comments are not carried.
func keepComments(src, dst *yaml.Node) {
	keepCommentsAt(src, dst, nil)
}

func keepCommentsAt(src, dst *yaml.Node, path []string) {
	if src == nil || dst == nil {
		return
	}
	dst.HeadComment, dst.LineComment, dst.FootComment = src.HeadComment, src.LineComment, src.FootComment
	if src.Kind != dst.Kind {
		return
	}
	switch src.Kind {
	case yaml.DocumentNode:
		if len(src.Content) == 1 && len(dst.Content) == 1 {
			keepCommentsAt(src.Content[0], dst.Content[0], path)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(src.Content); i += 2 {
			if j := keyIndex(dst, src.Content[i].Value); j >= 0 {
				keepComments(src.Content[i], dst.Content[j])
				keepCommentsAt(src.Content[i+1], dst.Content[j+1], append(path, src.Content[i].Value))
			}
		}
	case yaml.SequenceNode:
		signature := itemSignature
		if len(path) == 2 && path[0] == "workflow" && path[1] == "business_stores" {
			signature = businessStoreSignature
		}
		for i, item := range src.Content {
			if j := matchingItem(src, dst, i, signature); j >= 0 {
				keepCommentsAt(item, dst.Content[j], path)
			}
		}
	}
}

// keyIndex is the index of the key named name among m's entries, -1 when m
// has none.
func keyIndex(m *yaml.Node, name string) int {
	for j := 0; j+1 < len(m.Content); j += 2 {
		if m.Content[j].Kind == yaml.ScalarNode && m.Content[j].Value == name {
			return j
		}
	}
	return -1
}

// matchingItem is the index of dst's item that src's i-th item is: the one
// item of dst saying what it says when exactly one item on each side does,
// -1 otherwise.
func matchingItem(src, dst *yaml.Node, i int, signature func(*yaml.Node) string) int {
	sig := signature(src.Content[i])
	if sig == "" || count(src, sig, signature) != 1 || count(dst, sig, signature) != 1 {
		return -1
	}
	for j, item := range dst.Content {
		if signature(item) == sig {
			return j
		}
	}
	return -1
}

func count(seq *yaml.Node, sig string, signature func(*yaml.Node) string) int {
	n := 0
	for _, item := range seq.Content {
		if signature(item) == sig {
			n++
		}
	}
	return n
}

// A store's identity is the full tuple, regardless of the source's key order.
// Quoted components keep arbitrary separators and bytes unambiguous. An absent
// namespace and an explicitly empty one describe the same store.
func businessStoreSignature(n *yaml.Node) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	var values [3]string
	for i, key := range []string{"kind", "namespace", "id"} {
		j := keyIndex(n, key)
		if j < 0 {
			if key == "id" {
				return ""
			}
			continue
		}
		if n.Content[j+1].Kind != yaml.ScalarNode {
			return ""
		}
		values[i] = n.Content[j+1].Value
		if n.Content[j+1].Style == yaml.LiteralStyle {
			// Match the literal value read by speller.blockBreaks. Quoted
			// separators remain literal characters and identify another store.
			values[i] = blockSeparators.Replace(values[i])
		}
	}
	return fmt.Sprintf("business-store:%q/%q/%q", values[0], values[1], values[2])
}

// itemSignature is what a sequence item says about itself: a scalar its
// value, a mapping its first entry (`agent: hello` — the kind and name of a
// node, the name of a group or a route); "" for an item that says neither,
// which matches nothing.
func itemSignature(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return "scalar:" + n.Value
	case yaml.MappingNode:
		if len(n.Content) >= 2 && n.Content[0].Kind == yaml.ScalarNode && n.Content[1].Kind == yaml.ScalarNode {
			return "entry:" + n.Content[0].Value + "=" + n.Content[1].Value
		}
	}
	return ""
}
