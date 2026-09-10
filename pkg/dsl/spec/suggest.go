package spec

import (
	"sort"
	"strings"
)

// Suggest returns the kind's property names closest to name — at most
// three, nearest first — or nothing when none is close: an edit distance of
// one for a short name, two from five characters on, and a name that is a
// prefix of the other (a truncated `max_cost` for `max_cost_usd`).
func Suggest(kind, name string) []string {
	k, ok := Lookup(kind)
	if !ok {
		return nil
	}
	type cand struct {
		name string
		dist int
	}
	var out []cand
	for _, p := range k.Properties {
		if p.Name == name {
			return nil
		}
		d := levenshtein(name, p.Name)
		limit := 1
		if len(name) >= 5 {
			limit = 2
		}
		prefixed := len(name) >= 3 && (strings.HasPrefix(p.Name, name) || strings.HasPrefix(name, p.Name))
		if d <= limit || prefixed {
			out = append(out, cand{p.Name, d})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		return out[i].name < out[j].name
	})
	if len(out) > 3 {
		out = out[:3]
	}
	names := make([]string, 0, len(out))
	for _, c := range out {
		names = append(names, c.name)
	}
	return names
}

// UnknownPropertyHint is UnknownPropertyHintIn for a caller that does not
// know which host the block sits in.
func UnknownPropertyHint(kind, name string) string {
	return UnknownPropertyHintIn(kind, "", name)
}

// UnknownPropertyHintIn is the one-line remedy the parser attaches to an
// "unknown <kind> property" diagnostic: the closest accepted names when
// there are any; otherwise the block of this kind the name belongs to (a
// `mode:` written on the sandbox is the network's), or the enclosing kind
// it belongs to (an `entry:` written inside `budget:` is the workflow's);
// and always the kind's own list, so the author does not open the
// reference. host is the kind whose body the block sits in, when the
// caller knows it: an `mcp:` block lives under a workflow OR an agent, and
// "outdent it to the agent's level" is a lie under a workflow — so with an
// unknown host the enclosing-kind remedy is given only for a block with one
// possible host. Empty for a kind the registry does not know.
func UnknownPropertyHintIn(kind, host, name string) string {
	k, ok := Lookup(kind)
	if !ok {
		return ""
	}
	var parts []string
	switch {
	case len(Suggest(kind, name)) > 0:
		parts = append(parts, "Did you mean "+quoteList(Suggest(kind, name), " or ")+"?")
	case len(childOwners(k, name)) > 0:
		c := childOwners(k, name)
		parts = append(parts, "`"+name+"` is a property of the `"+c[0].Opener+":` block — indent it under `"+c[0].Opener+":`.")
	case enclosingOwner(k, host, name) != "":
		h := enclosingOwner(k, host, name)
		parts = append(parts, "`"+name+"` belongs to the enclosing `"+h+"`, not to `"+kind+"` — outdent it to the "+h+"'s level.")
	case len(Owners(name)) > 0:
		parts = append(parts, "`"+name+"` is a property of "+strings.Join(Owners(name), "/")+"; "+kind+" does not take it.")
	}
	if len(k.Properties) > 0 {
		parts = append(parts, kind+" accepts: "+strings.Join(k.Names(), ", ")+".")
	} else if k.Entries != nil {
		parts = append(parts, kind+" takes entries of the form `"+k.Entries.Shape+"`.")
	}
	return strings.Join(parts, " ")
}

// childOwners returns the kinds hosted by k that accept name.
func childOwners(k Kind, name string) []Kind {
	var out []Kind
	for _, c := range HostedBy(k.Name) {
		if c.Has(name) {
			out = append(out, c)
		}
	}
	return out
}

// enclosingOwner returns the host of k that accepts name — the host the
// block actually sits in when the caller named one that k lists, else k's
// only host; "" when the host is unknown among several (a guess would be a
// false claim) or accepts no such property.
func enclosingOwner(k Kind, host, name string) string {
	candidates := k.Hosts
	switch {
	case host != "" && hostOf(k, host):
		candidates = []string{host}
	case len(k.Hosts) != 1:
		return ""
	}
	for _, h := range candidates {
		if hk, ok := Lookup(h); ok && hk.Has(name) {
			return h
		}
	}
	return ""
}

func hostOf(k Kind, host string) bool {
	for _, h := range k.Hosts {
		if h == host {
			return true
		}
	}
	return false
}

func quoteList(names []string, sep string) string {
	q := make([]string, 0, len(names))
	for _, n := range names {
		q = append(q, "`"+n+"`")
	}
	return strings.Join(q, sep)
}

// levenshtein is the edit distance between two short ASCII-ish strings.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
