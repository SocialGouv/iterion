package spec_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// hostProbes places each multi-host block under EACH of its hosts, with a
// `%s` where an unknown property line goes. They exist for one reason: the
// remedy "outdent it to the <host>'s level" is a lie unless the parser
// recorded which host the block sits in (enterBlock), and nothing else would
// turn red for a multi-host block added without that call.
var hostProbes = map[string]map[string]string{
	"mcp": {
		"workflow": "workflow w:\n  mcp:\n    %s: 1\n",
		"agent":    "agent a:\n  mcp:\n    %s: 1\n",
		"judge":    "judge j:\n  mcp:\n    %s: 1\n",
	},
	"sandbox": {
		"workflow": "workflow w:\n  sandbox:\n    %s: 1\n",
		"agent":    "agent a:\n  sandbox:\n    %s: 1\n",
		"judge":    "judge j:\n  sandbox:\n    %s: 1\n",
		"tool":     "tool t:\n  sandbox:\n    %s: 1\n",
	},
	"compaction": {
		"workflow": "workflow w:\n  compaction:\n    %s: 1\n",
		"agent":    "agent a:\n  compaction:\n    %s: 1\n",
		"judge":    "judge j:\n  compaction:\n    %s: 1\n",
	},
	"memory": {
		"agent": "agent a:\n  memory:\n    %s: 1\n",
		"judge": "judge j:\n  memory:\n    %s: 1\n",
	},
	"fallback": {
		"agent": "agent a:\n  fallbacks:\n    r:\n      %s: 1\n",
		"judge": "judge j:\n  fallbacks:\n    r:\n      %s: 1\n",
	},
}

// borrowed returns a property of `from` that neither the block nor `not`
// accepts and that is not a near-miss of one of the block's own names (the
// closest-name remedy would then pre-empt the host logic and the probe
// would prove nothing) — the name whose remedy must point at `from` and
// nowhere else — or "" when the two hosts share every property (agent and
// judge), where no name can tell them apart and the remedy is right for both.
func borrowed(block, from, not spec.Kind) string {
	for _, p := range from.Properties {
		if !block.Has(p.Name) && !not.Has(p.Name) && len(spec.Suggest(block.Name, p.Name)) == 0 {
			return p.Name
		}
	}
	return ""
}

func e012Hint(t *testing.T, doc string) string {
	t.Helper()
	res := parser.Parse("probe.bot", doc)
	for _, d := range res.Diagnostics {
		if d.Code == parser.DiagUnknownProperty {
			return d.Hint
		}
	}
	t.Fatalf("no E012 in %q: %v", doc, res.Diagnostics)
	return ""
}

// TestTheEnclosingKindRemedyNamesTheHostTheBlockIsIn: under each host, a
// property of ANOTHER host draws no "enclosing <other>" remedy, and a
// property of THIS host draws exactly that one.
func TestTheEnclosingKindRemedyNamesTheHostTheBlockIsIn(t *testing.T) {
	// A (block, host) pair with no qualifying name skips its assertion, so
	// the count is pinned: a registry change that removes the last candidate
	// of several pairs would otherwise turn the guard into a quiet no-op.
	asserted := 0
	defer func() {
		if asserted < 12 {
			t.Errorf("only %d host assertions ran — the probes have gone quiet, check hostProbes and borrowed()", asserted)
		}
	}()
	for kind, byHost := range hostProbes {
		block, ok := spec.Lookup(kind)
		if !ok {
			t.Fatalf("probe kind %q is not registered", kind)
		}
		for host, tmpl := range byHost {
			hk, ok := spec.Lookup(host)
			if !ok {
				t.Fatalf("%s: host %q is not registered", kind, host)
			}
			for _, other := range block.Hosts {
				if other == host {
					continue
				}
				otherKind, _ := spec.Lookup(other)
				name := borrowed(block, otherKind, hk)
				if name == "" {
					continue
				}
				hint := e012Hint(t, fmt.Sprintf(tmpl, name))
				asserted++
				if strings.Contains(hint, "enclosing `"+other+"`") {
					t.Errorf("%s under %s: %q sent the author to `%s`, a host the block is not in", kind, host, name, other)
				}
			}
			// The host's own property, borrowed from nowhere else and no
			// near-miss of the block's names.
			var own string
			for _, p := range hk.Properties {
				if !block.Has(p.Name) && len(spec.Suggest(block.Name, p.Name)) == 0 {
					own = p.Name
					break
				}
			}
			if own == "" {
				continue
			}
			asserted++
			if hint := e012Hint(t, fmt.Sprintf(tmpl, own)); !strings.Contains(hint, "enclosing `"+host+"`") {
				t.Errorf("%s under %s: %q should name the enclosing `%s`, got %q", kind, host, own, host, hint)
			}
		}
	}
}

// TestEveryMultiHostBlockHasHostProbes: a multi-host block added to the
// registry (or a host added to one) without its probes here would be a
// block nothing holds to the rule above.
func TestEveryMultiHostBlockHasHostProbes(t *testing.T) {
	for _, k := range spec.Kinds {
		if len(k.Hosts) < 2 || len(k.Properties) == 0 || k.Entries != nil {
			continue // free-entry blocks (vars, attachments, cursors) never raise E012
		}
		byHost := hostProbes[k.Name]
		for _, h := range k.Hosts {
			if _, ok := byHost[h]; !ok {
				t.Errorf("multi-host block %q has no probe under host %q", k.Name, h)
			}
		}
	}
}
