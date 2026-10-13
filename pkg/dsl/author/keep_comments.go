package author

import "gopkg.in/yaml.v3"

// keepComments carries the comments of src — a document as yaml.v3 read
// it — onto dst, the tree the writer built for the program src describes.
// A comment rides the node it hangs on: dst's node at the same path takes
// its head, line and foot comments. The path is read through the document
// root, a mapping's entries by key, and a sequence's items by what they say
// (itemSignature) — never by position alone, so a comment cannot land on a
// neighbour the writer put in its place. A node dst does not hold at the
// path keeps nothing, and its comments are not carried.
func keepComments(src, dst *yaml.Node) {
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
			keepComments(src.Content[0], dst.Content[0])
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(src.Content); i += 2 {
			if j := keyIndex(dst, src.Content[i].Value); j >= 0 {
				keepComments(src.Content[i], dst.Content[j])
				keepComments(src.Content[i+1], dst.Content[j+1])
			}
		}
	case yaml.SequenceNode:
		for i, item := range src.Content {
			if j := matchingItem(src, dst, i); j >= 0 {
				keepComments(item, dst.Content[j])
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
func matchingItem(src, dst *yaml.Node, i int) int {
	sig := itemSignature(src.Content[i])
	if sig == "" || count(src, sig) != 1 || count(dst, sig) != 1 {
		return -1
	}
	for j, item := range dst.Content {
		if itemSignature(item) == sig {
			return j
		}
	}
	return -1
}

func count(seq *yaml.Node, sig string) int {
	n := 0
	for _, item := range seq.Content {
		if itemSignature(item) == sig {
			n++
		}
	}
	return n
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
