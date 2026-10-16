package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// envDefaultValue reads the literal a `${VAR:-literal}` value falls back to —
// the way every bundle writes an overridable model or backend. A value with no
// literal (`${VAR}`, `{{vars.llm_backend}}`) names no model a reader could
// check, and yields "".
var envDefaultValue = regexp.MustCompile(`^\$\{[^:}]+:-([^}]*)\}$`)

// declaredLiteral is the model id a declaration ships when no environment
// override is set.
func declaredLiteral(v string) string {
	v = strings.TrimSpace(v)
	if m := envDefaultValue.FindStringSubmatch(v); m != nil {
		return strings.TrimSpace(m[1])
	}
	if strings.ContainsAny(v, "${") {
		return ""
	}
	return v
}

// canonicalModelID reads a spec the way the engine reads one: a routing
// provider's prefix is a route, not part of the id (`modelroute`, the single
// reading the cost estimator and the delegate backends share), so
// `anthropic/claude-opus-5-5` and `claude-opus-5-5` name ONE model and a page
// may write either. A gateway route keeps its prefix — a gateway id is
// whatever that gateway serves — and so does a slash a model id owns
// (`kimi-code/kimi-for-coding`, `meta-llama/Llama-3.3-70B`).
//
// Both sides of the check are read through it, so the guard compares models
// and not spellings: without it, `assessment/README.md` naming the very id
// all three of its agents declare was reported as naming a model the bundle
// does not declare.
func canonicalModelID(spec string) string {
	r := modelroute.Parse(spec)
	if r.Provider == "" || r.Gateway() || !modelroute.IsRoutingProvider(r.Provider) {
		return spec
	}
	return r.Wire
}

// declaredModelsByBundle lists, per bundle, the models its own workflows
// ship: every LLM node's `model:` and every `fallbacks:` route's, with the
// `${VAR:-…}` default read as the shipped value and the routing prefix read
// off by canonicalModelID.
func declaredModelsByBundle(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	add := func(bundle, spec string) {
		if m := declaredLiteral(spec); m != "" {
			out[bundle][canonicalModelID(m)] = true
		}
	}
	for _, sw := range shippedWorkflows(t) {
		bundle := strings.SplitN(sw.prefix, "/", 2)[0]
		if out[bundle] == nil {
			out[bundle] = map[string]bool{}
		}
		for _, n := range sw.wf.Nodes {
			llm, isLLM := n.(ir.LLMNode)
			if !isLLM {
				continue
			}
			add(bundle, llm.GetLLMFields().Model)
			for _, fb := range llm.GetFallbacks() {
				add(bundle, fb.Model)
			}
		}
	}
	return out
}

// TestNoBundleReadmeNamesAModelTheBundleDoesNotDeclare holds a bundle's README
// to the models that bundle actually runs.
//
// The README is a published surface, not an internal note: `botinstall`
// captures it at install (pkg/botinstall/inspect.go, readREADME) and
// `marketplace` ships it as a registry entry's `readme` for the studio detail
// panel — so an operator reads it to decide which credential the run will
// spend. feature-dev is why the guard exists: its plan reviewer is pinned to
// `kimi` + `kimi-code/kimi-for-coding` with no environment default, while its
// page described the gpt pair its six siblings declare — the sentence travels
// between these pages by copy-paste, which is also how the six got it right.
//
// The vocabulary is the catalogue's own: every model any shipped bundle
// declares, canonicalModelID-read, so a page is only ever caught naming a
// model that exists in the tree but not in that bundle. No hand-written list
// to widen, and a new model joins the check by being declared. Backends are
// deliberately NOT in it: measured, two README mentions of `claw` are prose a
// backend cannot be read off (docs-refresh documents `--var llm_backend=claw`
// over its `{{vars.llm_backend}}` node), so the backend half would report
// what it cannot judge.
//
// Spelling is not a claim: both sides go through canonicalModelID, and each
// matched span is masked before the shorter ids are tried, so a page naming
// its own model with its routing prefix — or with the prefix off — is clean
// either way. The guard exists to make a page name what the `.bot` declares;
// accusing a correct sentence would teach the opposite.
//
// An override example is not a claim about what the run spends: a page that
// spells `ITERION_VIBE_MODEL_EMIT=openai/gpt-6-sol` documents the operator's
// explicit choice, which the `.bot` need not declare (review-pr's converge is
// `${ITERION_VIBE_MODEL_EMIT:-claude-opus-5-5}` while its README offers the
// OpenAI route). The value of a `VAR=value` spelling is therefore masked
// before the ids are tried; prose keeps its claim.
var envAssignedValue = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*=\S+`)

func TestNoBundleReadmeNamesAModelTheBundleDoesNotDeclare(t *testing.T) {
	declared := declaredModelsByBundle(t)
	vocabulary := map[string]bool{}
	for _, models := range declared {
		for m := range models {
			vocabulary[m] = true
		}
	}
	ids := make([]string, 0, len(vocabulary))
	for m := range vocabulary {
		ids = append(ids, m)
	}
	// Longest first, so one span of the page is one claim: an id contained in
	// a longer one the page also names is not a second model named (masked
	// below), and the ordering decides which of the two is reported.
	sort.Slice(ids, func(i, j int) bool {
		if len(ids[i]) != len(ids[j]) {
			return len(ids[i]) > len(ids[j])
		}
		return ids[i] < ids[j]
	})

	readmes, err := filepath.Glob("*/README.md")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	named := 0
	for _, page := range readmes {
		bundle := filepath.Dir(page)
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		body := envAssignedValue.ReplaceAllStringFunc(string(raw), func(m string) string {
			return strings.Repeat("\x00", len(m))
		})
		for _, id := range ids {
			if !strings.Contains(body, id) {
				continue
			}
			named++
			if !declared[bundle][id] {
				offenders = append(offenders, fmt.Sprintf("%s names %s", page, id))
			}
			// What this id covered is read; a shorter id inside those bytes
			// is the same claim, not another model the page names.
			body = strings.ReplaceAll(body, id, strings.Repeat("\x00", len(id)))
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d bundle page(s) name a model the bundle does not declare — an operator reads "+
			"the page to know which credential the run spends, and the registry ships it as the "+
			"entry's readme. Name what the `.bot` declares:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// Floors, because a guard that recognises no model and reads no page
	// passes everything: the catalogue ships several model ids, every bundle
	// has a page, and the pages that name a model are this guard's subject.
	if len(ids) < 5 || len(readmes) < 25 || named < 3 {
		t.Fatalf("vocabulary=%d readmes=%d model mentions=%d, want >= 5 / 25 / 3 — the guard stopped seeing its own subject (%v)",
			len(ids), len(readmes), named, ids)
	}
	t.Logf("checked %d README(s) against %d declared model id(s), %d mention(s)", len(readmes), len(ids), named)
}

// TestCanonicalModelIDReadsARouteAndNotAModelIDsOwnSlash executes both
// directions of the reading the guard compares on, so a change to it is a red
// test: a routing provider's prefix comes off, and a slash a model id owns
// does not. The providers come from modelroute's own roster, so a vendor
// added there is read the same way here without anyone editing this list.
func TestCanonicalModelIDReadsARouteAndNotAModelIDsOwnSlash(t *testing.T) {
	for _, c := range []struct{ spec, want string }{
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"anthropic/claude-opus-5-5", "claude-opus-5-5"},
		{"openai/gpt-6-astra", "gpt-6-astra"},
		{"moonshot/kimi-k2", "kimi-k2"},
		// Not a route: the slash belongs to the id, so the whole spec is it.
		{"kimi-code/kimi-for-coding", "kimi-code/kimi-for-coding"},
		{"meta-llama/Llama-3.3-70B", "meta-llama/Llama-3.3-70B"},
		// A gateway id is whatever that gateway serves — never a vendor id.
		{"openai_compatible/scaleway-gpt-oss-120b", "openai_compatible/scaleway-gpt-oss-120b"},
	} {
		if got := canonicalModelID(c.spec); got != c.want {
			t.Errorf("canonicalModelID(%q) = %q, want %q", c.spec, got, c.want)
		}
	}
	for _, p := range modelroute.RoutingProviders() {
		if got := canonicalModelID(p + "/some-model"); got != "some-model" {
			t.Errorf("canonicalModelID(%q) = %q, want %q — modelroute routes on this provider, so its prefix is not part of the id", p+"/some-model", got, "some-model")
		}
	}
}
