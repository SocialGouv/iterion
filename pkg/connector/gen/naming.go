package gen

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// deriveName turns a vendor operation into iterion's `<resource>.<verb>` pair.
//
// The derivation is the package's most consequential piece of taste, because
// the resulting id is what a `.bot` quotes and what must survive a vendor's
// next release. Three rules, in order of how much they can be trusted:
//
//  1. The vendor's first TAG is the resource. It is the grouping the vendor's
//     own documentation uses, it changes rarely, and it is what a human
//     looking for "the issue operations" would search.
//  2. The operationId supplies the verb, with the resource stripped off its
//     front — `issueCreateComment` under tag `issue` reads `create_comment`,
//     not `issue_create_comment`, which would stutter in every call site.
//  3. With no operationId, the METHOD and the path's shape answer: a GET on a
//     collection is `list`, on an item `get`; POST is `create`; PUT/PATCH
//     `update`; DELETE `delete`, each suffixed by the path's last literal
//     segment when that adds meaning.
//
// An overlay pins any id whose derivation would move, which is the escape
// hatch that lets rule 2 stay simple.
func deriveName(tags []string, sourceID, method, path string) (resource, verb string) {
	resource = "api"
	if len(tags) > 0 && strings.TrimSpace(tags[0]) != "" {
		resource = snake(tags[0])
	}
	if sourceID != "" {
		verb = snake(stripResourcePrefix(sourceID, resource))
	}
	if verb == "" {
		verb = deriveVerbFromPath(method, path)
	}
	if verb == "" {
		verb = snake(method)
	}
	return resource, verb
}

// stripResourcePrefix removes the resource from the front of an operationId,
// comparing on the normalised form so `issueCreateComment`, `IssueCreate` and
// `issue_create` all lose the same prefix.
//
// It also strips a vendor's ABBREVIATION of the resource. Naming an operation
// `orgCreateTeam` under tag `organization`, or `repoDeleteKey` under
// `repository`, is a common house style, and an exact-match strip leaves
// `organization.org_create_team` — a stutter in every call site of every
// workflow. Measured on Forgejo: 214 of 506 operations are abbreviated this
// way, against 187 that spell the resource out.
//
// The abbreviation must be a PREFIX of the resource and at least three
// characters, so a coincidence cannot eat a real verb (`get` is not a prefix
// of `group`; `a` would never qualify), and the strip is refused when nothing
// would be left — `userGet` under tag `user` becomes `get`, never "".
func stripResourcePrefix(sourceID, resource string) string {
	normalised := snake(sourceID)
	if normalised == resource {
		return sourceID
	}
	// The WHOLE resource first, which is the only thing that works for a
	// multi-word one: cutting at the first underscore would match `secret`
	// against `secret_scanning` and leave `scanning_bulk_delete_…`, a verb
	// carrying half its own resource.
	if rest, ok := strings.CutPrefix(normalised, resource+"_"); ok && rest != "" {
		return rest
	}
	head, rest, found := strings.Cut(normalised, "_")
	if !found || rest == "" {
		return sourceID
	}
	if len(head) >= 3 && strings.HasPrefix(resource, head) {
		return rest
	}
	return sourceID
}

// deriveVerbFromPath is the fallback for a description with no operation ids.
func deriveVerbFromPath(method, path string) string {
	last := lastLiteralSegment(path)
	endsInParam := strings.HasSuffix(strings.TrimRight(path, "/"), "}")

	var base string
	switch strings.ToLower(method) {
	case "get":
		if endsInParam {
			base = "get"
		} else {
			base = "list"
		}
	case "post":
		base = "create"
	case "put", "patch":
		base = "update"
	case "delete":
		base = "delete"
	default:
		base = strings.ToLower(method)
	}
	if last == "" {
		return base
	}
	return base + "_" + snake(last)
}

// lastLiteralSegment returns the last non-templated path segment, which is
// what usually names the sub-resource an operation acts on.
func lastLiteralSegment(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		if p == "" || strings.HasPrefix(p, "{") {
			continue
		}
		return p
	}
	return ""
}

// effectFor classifies what an operation does to the remote system.
//
// The METHOD is the baseline, and the VERB corrects it for the one case that
// matters: a POST that searches. Vendors route search through POST whenever
// the query outgrows a URL (Jira's `/search`, Slack's message search), and
// calling those "create" would deny them the retry a read is entitled to —
// the whole reason Effect is a declared field rather than a lookup on the
// method at call time.
func effectFor(method, verb string) spec.Effect {
	switch strings.ToLower(method) {
	case "get", "head", "options":
		return spec.EffectRead
	case "delete":
		return spec.EffectDelete
	case "put", "patch":
		return spec.EffectUpdate
	case "post":
		if isReadVerb(verb) {
			return spec.EffectRead
		}
		return spec.EffectCreate
	}
	return spec.EffectRead
}

// isReadVerb recognises the verbs that name a read even under POST. It is a
// deliberately SHORT list of prefixes: a wrong "this is a read" grants a
// blind retry to something that mutates, so the derivation stays timid and
// the overlay states the rest.
func isReadVerb(verb string) bool {
	for _, p := range []string{"search", "list", "get", "find", "query", "check", "read", "lookup"} {
		if verb == p || strings.HasPrefix(verb, p+"_") {
			return true
		}
	}
	return false
}

// snake converts camelCase, PascalCase, kebab-case, dotted and spaced names
// into snake_case, collapsing runs of separators and dropping anything that
// is not a letter or a digit. It is what makes an id safe to quote in a
// `.bot`, in a URL segment and in an MCP tool name at once.
func snake(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	prevLower := false
	prevDigit := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			// A boundary before an upper-case run that follows a lower-case
			// letter or a digit: "createComment" → create_comment, and
			// "v2Token" → v2_token.
			if prevLower || prevDigit {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			prevLower = false
			prevDigit = false
		case (r >= 'a' && r <= 'z'):
			b.WriteRune(r)
			prevLower = true
			prevDigit = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevLower = false
			prevDigit = true
		default:
			// Any separator (space, '-', '.', '/', '_') collapses to one '_'.
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
			prevLower = false
			prevDigit = false
		}
	}
	return strings.Trim(b.String(), "_")
}
