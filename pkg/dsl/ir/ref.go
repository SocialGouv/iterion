package ir

import (
	"fmt"
	"strings"
)

// ParseRefs extracts all {{...}} template references from a string.
// Returns the parsed Ref values. Returns an error if a template
// expression is malformed.
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
		expr := strings.TrimSpace(rest[start+2 : end])
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
func parseRef(expr, raw string) (*Ref, error) {
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
	case "loop":
		kind = RefLoop
	case "run":
		kind = RefRun
	default:
		return nil, fmt.Errorf("unknown reference namespace %q in %q", namespace, raw)
	}

	return &Ref{
		Kind: kind,
		Path: path,
		Raw:  raw,
	}, nil
}
