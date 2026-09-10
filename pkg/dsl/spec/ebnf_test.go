package spec_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// ebnfProductions maps a registry kind to the EBNF production that lists
// its properties (docs/grammar/iterion_v1.ebnf). The EBNF stays
// hand-written — its structure is more than property lists — but each of
// these productions must name exactly the properties the registry does,
// or the machine-readable grammar teaches a surface the parser does not
// have.
var ebnfProductions = map[string]string{
	"agent":           "llm_prop",
	"judge":           "llm_prop",
	"router":          "router_prop",
	"human":           "human_prop",
	"tool":            "tool_prop",
	"recovery":        "recovery_prop",
	"compute":         "compute_prop",
	"emit":            "emit_prop",
	"wait":            "wait_prop",
	"await_answers":   "await_answers_prop",
	"fail":            "fail_prop",
	"subbot":          "subbot_prop",
	"workflow":        "workflow_member",
	"budget":          "budget_prop",
	"compaction":      "compaction_prop",
	"memory":          "memory_prop",
	"mcp":             "mcp_config_prop",
	"sandbox":         "sandbox_prop",
	"sandbox.build":   "sandbox_build_prop",
	"sandbox.network": "sandbox_network_prop",
	"mcp_server":      "mcp_server_prop",
	"auth":            "mcp_auth_prop",
	"cursor":          "cursor_prop",
	"supervisor":      "supervisor_prop",
	"attachment":      "attachment_prop",
	"secret":          "secret_prop",
	"fallback":        "fallback_prop",
}

var (
	productionRe = regexp.MustCompile(`(?m)^([a-z_]+)\s*=`)
	quotedRe     = regexp.MustCompile(`^"([a-z_]+)"`)
)

// loadProductions reads the EBNF into name → body (the text between `=`
// and the terminating `;`, comments stripped).
func loadProductions(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "grammar", "iterion_v1.ebnf"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if i := strings.Index(l, "(*"); i >= 0 && strings.Contains(l, "*)") {
			l = l[:i] + l[strings.Index(l, "*)")+2:]
		}
		lines = append(lines, l)
	}
	text := strings.Join(lines, "\n")
	out := map[string]string{}
	locs := productionRe.FindAllStringSubmatchIndex(text, -1)
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := text[loc[1]:end]
		if j := strings.LastIndex(body, ";"); j >= 0 {
			body = body[:j]
		}
		out[text[loc[2]:loc[3]]] = strings.TrimSpace(body)
	}
	if len(out) < 50 {
		t.Fatalf("only %d productions read from the EBNF", len(out))
	}
	return out
}

// alternatives splits a production body on its top-level `|`.
func alternatives(body string) []string {
	var out []string
	depth, inQuote := 0, false
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inQuote:
			if c == '"' {
				inQuote = false
			}
		case c == '"':
			inQuote = true
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == '|' && depth == 0:
			out = append(out, strings.TrimSpace(body[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(body[start:]))
}

// propertyNames lists the property each alternative of a production opens
// with: its leading quoted word, or, for an alternative that is another
// production, that production's leading quoted word (a block such as
// mcp_auth_block = "auth" ":" …). An alternative opening with neither (an
// edge) is not a property.
func propertyNames(prods map[string]string, name string) []string {
	var out []string
	for _, alt := range alternatives(prods[name]) {
		if m := quotedRe.FindStringSubmatch(alt); m != nil {
			out = append(out, m[1])
			continue
		}
		ref := strings.Fields(alt)
		if len(ref) == 0 {
			continue
		}
		if body, ok := prods[ref[0]]; ok {
			if m := quotedRe.FindStringSubmatch(strings.TrimSpace(body)); m != nil {
				out = append(out, m[1])
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestEBNFPropertyProductionsMatchTheRegistry(t *testing.T) {
	prods := loadProductions(t)
	for kind, prod := range ebnfProductions {
		k, ok := spec.Lookup(kind)
		if !ok {
			t.Errorf("%s: not a registered kind", kind)
			continue
		}
		if _, ok := prods[prod]; !ok {
			t.Errorf("%s: production %q not found in the EBNF", kind, prod)
			continue
		}
		got := propertyNames(prods, prod)
		want := append([]string(nil), k.Names()...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: EBNF production %s lists %v, the registry %v", kind, prod, got, want)
		}
	}
	for _, k := range spec.Kinds {
		if len(k.Properties) > 0 && k.Name != "cursors" {
			if _, ok := ebnfProductions[k.Name]; !ok {
				t.Errorf("kind %q has a property table but no EBNF production mapped", k.Name)
			}
		}
	}
}
