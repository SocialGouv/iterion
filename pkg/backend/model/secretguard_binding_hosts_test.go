package model

import (
	"context"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TestEffectiveSecretHosts pins the bot-secret-binding egress semantics:
// a binding can only NARROW the workflow's declared hosts, never broaden
// them, and disjoint policies deny all egress (NOT allow-any, which is
// what a naive empty intersection would have meant given secretguard's
// "empty Hosts == any host" rule).
func TestEffectiveSecretHosts(t *testing.T) {
	cases := []struct {
		name              string
		workflow, binding []string
		want              []string
	}{
		{"both empty -> unrestricted", nil, nil, nil},
		{"binding empty -> workflow unchanged", []string{"a.com"}, nil, []string{"a.com"}},
		{"workflow empty -> binding narrows from any", nil, []string{"b.com"}, []string{"b.com"}},
		{"overlap -> intersection", []string{"a.com", "b.com"}, []string{"b.com"}, []string{"b.com"}},
		{"disjoint -> deny all (not any)", []string{"a.com"}, []string{"b.com"}, []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveSecretHosts(c.workflow, c.binding)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("effectiveSecretHosts(%v,%v)=%v want %v", c.workflow, c.binding, got, c.want)
			}
		})
	}
}

// TestBuildSecretGuard_BindingHostsNarrowEgress proves the end-to-end
// wiring of a bot-secret binding's AllowedHosts: a workflow secret that
// declares NO hosts (egress open) is constrained to the binding's hosts
// once the runner threads them onto Credentials.GenericHosts. This is the
// exact case the review flagged — without the wiring an org credential
// bound to a bot was exfiltratable anywhere.
func TestBuildSecretGuard_BindingHostsNarrowEgress(t *testing.T) {
	const val = "glpat-SPECIMEN-abcdef0123456789wxyz"
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {}, // empty value -> resolved from Generic; no workflow hosts
	}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Generic:      map[string]string{"forge_token": val},
		GenericHosts: map[string][]string{"forge_token": {"gitlab.com"}},
	})
	g := BuildSecretGuard(ctx, wf, nil, nil)
	if g == nil {
		t.Fatal("guard is nil; a known secret should yield a guard")
	}
	if !g.ExfiltratesTo(val, "evil.com") {
		t.Error("binding host gitlab.com must restrict egress: evil.com should be flagged as exfiltration")
	}
	if g.ExfiltratesTo(val, "gitlab.com") {
		t.Error("approved binding host gitlab.com must not be flagged")
	}
	// api.gitlab.com is a sub-domain of an approved host → allowed.
	if g.ExfiltratesTo(val, "api.gitlab.com") {
		t.Error("sub-domain of an approved host must not be flagged")
	}
}

// TestBuildSecretGuard_DisjointHostsDenyAll proves disjoint workflow vs
// binding host policies deny ALL egress (never broaden to "any"). A naive
// empty intersection would have meant allow-everywhere under secretguard's
// empty==any rule — the regression effectiveSecretHosts guards against.
func TestBuildSecretGuard_DisjointHostsDenyAll(t *testing.T) {
	const val = "glpat-SPECIMEN-fedcba9876543210stuv"
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {Value: val, Hosts: []string{"example.com"}},
	}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		GenericHosts: map[string][]string{"forge_token": {"gitlab.com"}},
	})
	g := BuildSecretGuard(ctx, wf, nil, nil)
	if g == nil {
		t.Fatal("guard is nil; a known secret should yield a guard")
	}
	// Even the workflow's own declared host is now out of scope, and egress
	// is NOT broadened to any host.
	if !g.ExfiltratesTo(val, "example.com") {
		t.Error("disjoint policy must deny all egress (example.com must be flagged)")
	}
	if !g.ExfiltratesTo(val, "gitlab.com") {
		t.Error("disjoint policy must deny all egress (gitlab.com must be flagged)")
	}
}
