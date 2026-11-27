package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	cloudmetrics "github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The chart's starter alerts read iterion_launch_denied_total and
// iterion_webhook_throttled_total through rate() and increase(), so a fresh
// registry creates their children at 0 (cloudmetrics.New): a child born by
// its first Inc is first scraped at 1, and the rule never sees that event.
// The registry restates this package's label values to stay a leaf; these
// tests pin the restatement to the source.

// servedChildren is every child of vec a fresh registry serves, keyed by the
// values of the labels named, joined by "/", with its value.
func servedChildren(t *testing.T, vec *prometheus.CounterVec, labels ...string) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	vec.Collect(ch)
	close(ch)
	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("read a counter child: %v", err)
		}
		values := make([]string, len(labels))
		for _, l := range pb.GetLabel() {
			if i := slices.Index(labels, l.GetName()); i >= 0 {
				values[i] = l.GetValue()
			}
		}
		out[strings.Join(values, "/")] = pb.GetCounter().GetValue()
	}
	return out
}

// parsedSourceFiles parses this package's non-test Go files.
func parsedSourceFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, f)
	}
	return out
}

// denialTokens reads the deny* string constants of this package's source:
// the admission gate's denial tokens, by name.
func denialTokens(t *testing.T, files []*ast.File) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "deny") || len(n.Name) == len("deny") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", n.Name, err)
					}
					out[n.Name] = v
				}
			}
		}
	}
	return out
}

// Every token a launch denial carries is a deny* constant — read from the
// source: each launchDenial literal's reason and each forkClosedDenial call's
// first argument — and a fresh registry serves every one of them at 0, and
// nothing else. Red when a denial token is added without its child (its
// first refusal would be invisible to IterionLaunchDeniesSpiking), when the
// registry keeps a token the gate no longer has, or when a denial carries a
// reason that is not a deny* constant.
func TestLaunchDenialTokens_AreServedAtZeroByAFreshRegistry(t *testing.T) {
	files := parsedSourceFiles(t)
	tokens := denialTokens(t, files)
	for _, want := range []string{"denyOrgSuspended", "denyMemberRoleRequired", "denyMaintenanceFence"} {
		if _, ok := tokens[want]; !ok {
			t.Fatalf("%s is not read as a denial token: the source scan is broken", want)
		}
	}
	literals := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				var reason ast.Expr
				switch n := n.(type) {
				case *ast.CompositeLit:
					if id, ok := n.Type.(*ast.Ident); !ok || id.Name != "launchDenial" {
						return true
					}
					literals++
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "reason" {
								reason = kv.Value
							}
						}
					}
					if reason == nil {
						t.Errorf("%s: a launchDenial literal carries no reason", fd.Name.Name)
						return true
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "forkClosedDenial" || len(n.Args) == 0 {
						return true
					}
					reason = n.Args[0]
				default:
					return true
				}
				id, ok := reason.(*ast.Ident)
				if ok && id.Name == "reason" && fd.Name.Name == "forkClosedDenial" {
					return true
				}
				if !ok {
					t.Errorf("%s: a launch denial's reason is not a deny* constant", fd.Name.Name)
					return true
				}
				if _, known := tokens[id.Name]; !known {
					t.Errorf("%s: a launch denial's reason %s is not a deny* constant", fd.Name.Name, id.Name)
				}
				return true
			})
		}
	}
	if literals == 0 {
		t.Fatal("no launchDenial literal read: the source scan is broken")
	}
	served := servedChildren(t, cloudmetrics.New().LaunchDeniedTotal, "reason")
	for name, token := range tokens {
		if v, ok := served[token]; !ok || v != 0 {
			t.Errorf("%s (%q): a fresh registry serves (%v, %v), want 0 — its first refusal would be invisible to rate()", name, token, v, ok)
		}
	}
	for token := range served {
		found := false
		for _, v := range tokens {
			found = found || v == token
		}
		if !found {
			t.Errorf("a fresh registry serves the reason %q, which no deny* constant names", token)
		}
	}
}

// Every provider the webhook API accepts is served at 0 by a fresh registry
// under each throttle reason, and no other provider is. Red when a provider
// is added without its children (its first throttled delivery would be
// invisible to IterionWebhookThrottling's increase()).
func TestWebhookThrottleProviders_AreServedAtZeroByAFreshRegistry(t *testing.T) {
	served := servedChildren(t, cloudmetrics.New().WebhookThrottledTotal, "provider", "reason")
	reasons := []string{cloudmetrics.WebhookThrottledRateLimited, cloudmetrics.WebhookThrottledQuotaExceeded, cloudmetrics.WebhookThrottledBacklogFull}
	if len(supportedProviders) == 0 {
		t.Fatal("no supported webhook provider")
	}
	for provider := range supportedProviders {
		for _, reason := range reasons {
			if v, ok := served[string(provider)+"/"+reason]; !ok || v != 0 {
				t.Errorf("%s/%s: a fresh registry serves (%v, %v), want 0", provider, reason, v, ok)
			}
		}
	}
	if want := len(supportedProviders) * len(reasons); len(served) != want {
		t.Errorf("a fresh registry serves %d throttle children %v, want %d (each accepted provider under each reason)", len(served), served, want)
	}
}
