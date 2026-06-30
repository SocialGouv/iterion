package ir

import (
	"fmt"
	"strings"
)

// ParseRefs extracts all {{...}} template references from a string.
// Returns the parsed Ref values. Returns an error if a template
// expression is malformed or nests another {{ inside an open block.
//
// Nested templates like "{{outer{{inner}}}}" used to silently match
// the first `}}` as the closing fence, producing "outer{{inner" as
// the expression and a confusing "unknown namespace" error. The
// pre-scan for a nested `{{` now surfaces the real problem.
func ParseRefs(s string) ([]*Ref, error) {
	var refs []*Ref
	rest := s
	for {
		start := strings.Index(rest, "{{")
		if start == -1 {
			break
		}
		end := strings.Index(rest[start:], "}}")
		if end == -1 {
			return nil, fmt.Errorf("unterminated template expression at %q", rest[start:])
		}
		end += start // adjust to absolute position
		inner := rest[start+2 : end]
		if nested := strings.Index(inner, "{{"); nested != -1 {
			return nil, fmt.Errorf("nested template expression at %q: '{{' may not appear inside an open template block", rest[start:end+2])
		}
		expr := strings.TrimSpace(inner)
		raw := rest[start : end+2]
		ref, err := parseRef(expr, raw)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
		rest = rest[end+2:]
	}
	return refs, nil
}

// parseRef parses a single template expression like "outputs.node.field".
//
// A leading `!` flags the reference as "unquoted" (raw substitution) for
// tool command rendering. `{{!input.cmd}}` substitutes input.cmd verbatim
// into the resolved shell command instead of running through shellEscape.
// The bang prefix is stripped before namespace parsing so the rest of the
// expression behaves exactly as without it. The flag has no effect on
// non-shell template contexts (prompts, edge data mappings) — they always
// render values via formatValue.
func parseRef(expr, raw string) (*Ref, error) {
	unquoted := false
	if strings.HasPrefix(expr, "!") {
		unquoted = true
		expr = strings.TrimSpace(expr[1:])
	}
	parts := strings.Split(expr, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid reference %q: expected namespace.path", raw)
	}
	namespace := parts[0]
	path := parts[1:]

	var kind RefKind
	switch namespace {
	case "vars":
		kind = RefVars
	case "input":
		kind = RefInput
	case "outputs":
		kind = RefOutputs
	case "artifacts":
		kind = RefArtifacts
	case "attachments":
		kind = RefAttachments
	case "loop":
		kind = RefLoop
	case "each":
		kind = RefEach
	case "run":
		kind = RefRun
	case "secrets":
		kind = RefSecrets
	default:
		return nil, fmt.Errorf("unknown reference namespace %q in %q", namespace, raw)
	}

	return &Ref{
		Kind:     kind,
		Path:     path,
		Raw:      raw,
		Unquoted: unquoted,
	}, nil
}
