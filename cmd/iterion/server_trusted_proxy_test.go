package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The warning is the only thing standing between an upgrade and a silent
// availability change: with no trusted range, a deployment behind an
// ingress keys every request on the controller's own address, so the auth
// rate limiter holds one bucket for the whole instance (#2673). It must
// name the variable and that consequence, and it must fall silent once
// the operator has named the ranges.
func TestTrustedProxyWarning(t *testing.T) {
	t.Run("unset: the warning names the variable and the one-bucket cost", func(t *testing.T) {
		got := trustedProxyWarning(nil)
		for _, want := range []string{"ITERION_AUTH_TRUSTED_PROXY_CIDRS", "one bucket", "docs/environment-variables.md"} {
			if !strings.Contains(got, want) {
				t.Errorf("warning %q does not carry %q", got, want)
			}
		}
	})

	t.Run("set: no warning", func(t *testing.T) {
		if got := trustedProxyWarning([]string{"10.42.0.0/16"}); got != "" {
			t.Errorf("got %q, want no warning once the ranges are named", got)
		}
	})

	// The other end of the knob, and the louder one: config.Validate
	// refuses `10.42.0.7/16` for widening trust by a typo to a whole
	// /16, while `0.0.0.0/0` widens it to the internet and passes —
	// nginx's own `set_real_ip_from 0.0.0.0/0` is the canonical value
	// operators copy. Every client is then a proxy the operator vouches
	// for: the audit IP and the rate-limit key are whatever the request
	// says. Warned, never refused (philosophy 1).
	t.Run("a /0 entry: the warning names it and what it costs", func(t *testing.T) {
		got := trustedProxyWarning([]string{"10.42.0.0/16", "0.0.0.0/0"})
		for _, want := range []string{"0.0.0.0/0", "trusted to name its own address", "docs/environment-variables.md"} {
			if !strings.Contains(got, want) {
				t.Errorf("warning %q does not carry %q", got, want)
			}
		}
	})

	// Both halves of a dual-stack footgun, or the operator fixes one and
	// keeps the other.
	t.Run("a /0 on both families names both", func(t *testing.T) {
		got := trustedProxyWarning([]string{"0.0.0.0/0", "::/0"})
		for _, want := range []string{"0.0.0.0/0", "::/0"} {
			if !strings.Contains(got, want) {
				t.Errorf("warning %q does not carry %q", got, want)
			}
		}
	})
}

// #2673's first half was a field declared and wired nowhere: the server
// held TrustedProxyCIDRs, no caller ever filled it, and the limiter kept
// keying on the socket. `iterion server` is the deployment command and
// BOTH its modes are shapes behind a Service: the cloud branch builds the
// server.Config itself, the local branch hands the ranges to RunStudio,
// which puts them in its own literal.
//
// pkg/cli holds TWO server.Config builders and RunStudio picks between
// them before it reaches its own: `if opts.Workspace` returns into
// workspace_studio.go, so a field wired in one of the two is wired for
// one of the two shapes.
//
// Each literal is read from the AST, not grepped from the file, and EVERY
// literal of the type must carry the field — `wired == literals`, never
// "some literal somewhere did": two builders in one file would otherwise
// cover each other, exactly as a guard that greps the file for the
// assignment stays green while either one of its literals loses it.
//
// The THREE halves of the decision travel together, down the same four
// literals: the ranges say WHO may name the client, TrustedProxyHops how
// many of the chain's rightmost entries are theirs, ForwardedHeader in
// WHICH header — and a shape that gets some without the others reads a
// client address from a mechanism the operator never declared, or stops
// the walk at a hop they never declared, while the logs say the ranges
// are set. A count that reaches three builders out of four is the same
// dead field as a range that reaches none: the shape it misses keys
// every client on one address and nothing says so.
func TestEveryServerBuilderCarriesTheForwardedTrustSettings(t *testing.T) {
	for _, c := range []struct{ path, lit, field, value string }{
		{"server.go", "server.Config", "TrustedProxyCIDRs", "cfg.Auth.TrustedProxyCIDRs"},
		{"server.go", "cli.StudioOptions", "TrustedProxyCIDRs", "cfg.Auth.TrustedProxyCIDRs"},
		{"../../pkg/cli/studio.go", "server.Config", "TrustedProxyCIDRs", "opts.TrustedProxyCIDRs"},
		{"../../pkg/cli/workspace_studio.go", "server.Config", "TrustedProxyCIDRs", "opts.TrustedProxyCIDRs"},
		{"server.go", "server.Config", "TrustedProxyHops", "cfg.Auth.TrustedProxyHops"},
		{"server.go", "cli.StudioOptions", "TrustedProxyHops", "cfg.Auth.TrustedProxyHops"},
		{"../../pkg/cli/studio.go", "server.Config", "TrustedProxyHops", "opts.TrustedProxyHops"},
		{"../../pkg/cli/workspace_studio.go", "server.Config", "TrustedProxyHops", "opts.TrustedProxyHops"},
		{"server.go", "server.Config", "ForwardedHeader", "cfg.Auth.ForwardedHeader"},
		{"server.go", "cli.StudioOptions", "ForwardedHeader", "cfg.Auth.ForwardedHeader"},
		{"../../pkg/cli/studio.go", "server.Config", "ForwardedHeader", "opts.ForwardedHeader"},
		{"../../pkg/cli/workspace_studio.go", "server.Config", "ForwardedHeader", "opts.ForwardedHeader"},
	} {
		t.Run(c.path+" "+c.lit+" "+c.field, func(t *testing.T) {
			src, err := os.ReadFile(c.path)
			if err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, c.path, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			text := func(n ast.Node) string {
				return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
			}
			literals, wired := 0, 0
			var bare []int
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || lit.Type == nil || text(lit.Type) != c.lit {
					return true
				}
				literals++
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == c.field && strings.TrimSpace(text(kv.Value)) == c.value {
						wired++
						return true
					}
				}
				bare = append(bare, fset.Position(lit.Pos()).Line)
				return true
			})
			if literals == 0 {
				t.Fatalf("%s builds no %s — this guard reads the wrong file", c.path, c.lit)
			}
			if wired != literals {
				t.Errorf("%s builds %d %s literals and only %d pass %s: %s — the ones at lines %v never see what the operator configured, which is the half of #2673 that made the field dead", c.path, literals, c.lit, wired, c.field, c.value, bare)
			}
		})
	}
}
