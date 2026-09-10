package ir

import (
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// compileToolAction compiles the connector-action recipe of a tool node and
// enforces what makes it DETERMINISTIC.
//
// The refusals here are not style. An action node's whole promise is that no
// model decides the operation, builds the arguments or reads the answer — so
// every property that could smuggle one back in is refused at compile time,
// where an author sees it, rather than at run time, where a run has already
// spent money reaching a third party.
func (c *compiler) compileToolAction(t *ast.ToolNodeDecl) []ActionParam {
	if t.Action == "" {
		// A property that only means something alongside `action:` is INERT
		// here, and an inert property reads as configured — which is how a
		// node ends up looking bound to a connection it never uses.
		for _, orphan := range []struct {
			name string
			set  bool
		}{
			{"connection", t.Connection != ""},
			{"params", len(t.Params) > 0},
			{"retry", t.Retry != ""},
		} {
			if orphan.set {
				c.warnfAt(DiagActionOnlyProperty, t.Name, "",
					"tool %q: `%s:` has no effect without `action:` — it is read only by the connector recipe", t.Name, orphan.name)
			}
		}
		return nil
	}

	if !validActionID(t.Action) {
		c.errorfAt(DiagActionMalformedID, t.Name, "",
			"tool %q: `action: %s` is not an operation id — it must read `connector.resource.verb`, e.g. `forgejo.issue.comment`", t.Name, t.Action)
	}
	if strings.TrimSpace(t.Connection) == "" {
		c.errorfAt(DiagActionNoConnection, t.Name, "",
			"tool %q: `action:` needs a `connection:` — a connector call with no credential is not a call iterion can make", t.Name)
	}

	// ADR-044's ladder ends in an LLM repairing the recipe. On a deterministic
	// action that is precisely the thing the offer promises does not happen,
	// so it is refused rather than left to an operator to avoid.
	if t.Recovery != nil || t.Policy == PolicyRecover {
		c.errorfAt(DiagActionRecovery, t.Name, "",
			"tool %q: `recovery:` / `policy: recover` cannot apply to an `action:` — its recovery ladder ends in an LLM repairing the call, "+
				"which is exactly what a deterministic node promises does not happen", t.Name)
	}
	// A postcondition is a SHELL command judging success. On an action the
	// vendor's own typed answer is the truth, and letting a shell exit code
	// overrule it would let a node report success on a call that failed.
	if t.Postcondition != "" {
		c.errorfAt(DiagActionPostcond, t.Name, "",
			"tool %q: `postcondition:` cannot apply to an `action:` — the operation's typed result is its success oracle, "+
				"and a shell exit code would overrule what the vendor actually answered", t.Name)
	}

	if t.Timeout != "" {
		if _, err := time.ParseDuration(t.Timeout); err != nil {
			c.errorfAt(DiagActionBadTimeout, t.Name, "",
				"tool %q: `timeout: %s` is not a duration (want e.g. 30s, 2m)", t.Name, t.Timeout)
		}
	}
	if t.Retry != "" {
		if _, err := time.ParseDuration(t.Retry); err != nil && !isPositiveInteger(t.Retry) {
			c.errorfAt(DiagActionBadTimeout, t.Name, "",
				"tool %q: `retry: %s` is neither an attempt count nor a duration", t.Name, t.Retry)
		}
	}

	seen := make(map[string]bool, len(t.Params))
	out := make([]ActionParam, 0, len(t.Params))
	for _, p := range t.Params {
		key := strings.TrimSpace(p.Key)
		if key == "" {
			c.errorfAt(DiagActionBadParam, t.Name, "", "tool %q: a `params:` entry has no name", t.Name)
			continue
		}
		if seen[key] {
			// Silently keeping one would send a value the author did not
			// write, with nothing to notice it.
			c.errorfAt(DiagActionBadParam, t.Name, "", "tool %q: `params:` declares %q twice", t.Name, key)
			continue
		}
		seen[key] = true

		refs, err := ParseRefs(p.Value)
		if err != nil {
			c.errorfAt(DiagBadTemplateRef, t.Name, "", "tool %q param %q: %v", t.Name, key, err)
		}
		out = append(out, ActionParam{Key: key, Value: p.Value, Refs: refs})
	}
	return out
}

// validActionID checks the `connector.resource.verb` shape. It is a SHAPE
// check only: whether the operation exists is a question for the connector
// package, which `iterion validate` resolves separately and a compile cannot.
func validActionID(id string) bool {
	parts := strings.Split(id, ".")
	if len(parts) < 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || !isLowerSnakeSegment(p) {
			return false
		}
	}
	return true
}

// isLowerSnakeSegment accepts the segment shape the generator produces:
// lowercase letters, digits and underscores, starting with a letter.
func isLowerSnakeSegment(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r == '_' || (r >= '0' && r <= '9'):
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return s != ""
}

func isPositiveInteger(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != "0"
}
