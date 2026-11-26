package forge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// TestCatalogCommandsRouteToTheirBot provisions the whole shipped catalog on
// one repository, as an operator enabling every bot would, and drives forge
// comments through the shared parser and resolver. The three renamed bots
// that declare a command answer to the new one and keep the old one
// (docs/agents/bots/naming.md), and no two catalog bots claim the same
// command without args disambiguation.
func TestCatalogCommandsRouteToTheirBot(t *testing.T) {
	var paths []string
	for _, name := range []string{bundle.ManifestFile, bundle.ManifestFileAlt} {
		found, err := filepath.Glob(filepath.Join("..", "..", "bots", "*", name))
		if err != nil {
			t.Fatalf("glob the catalog manifests (%s): %v", name, err)
		}
		paths = append(paths, found...)
	}
	if len(paths) < 30 {
		t.Fatalf("found %d catalog manifests, want at least 30: the walk is not reaching bots/", len(paths))
	}
	invocations := map[string][]bundle.Invocation{}
	var bots []string
	for _, p := range paths {
		m, err := bundle.LoadManifest(p)
		if err != nil {
			t.Fatalf("%s: load manifest: %v", p, err)
		}
		if m == nil {
			t.Fatalf("%s: no manifest loaded", p)
		}
		botID := filepath.Base(filepath.Dir(p))
		invocations[botID] = m.Invocations
		bots = append(bots, botID)
	}

	o := &Orchestrator{Invocations: invLookup(invocations)}
	commands, err := o.buildCommandMap(context.Background(), "t1", bots)
	if err != nil {
		t.Fatalf("provisioning the whole catalog on one repository: %v", err)
	}
	cfg := webhooks.Config{CommandMap: commands}

	for _, tc := range []struct{ comment, bot string }{
		{"/white-elephant ADR-012", "adr-rechallenge"},
		{"/rearchi ADR-012", "adr-rechallenge"},
		{"/devboxing", "devbox-setup"},
		{"/devy", "devbox-setup"},
		{"/code-red", "supply-shield-cve"},
		{"/cve", "supply-shield-cve"},
	} {
		cmd, args := webhooks.ParseSlashCommand(tc.comment)
		route, ok := webhooks.ResolveCommandRoute(cfg, cmd, args, nil)
		if !ok {
			t.Errorf("comment %q: /%s resolves to no bot", tc.comment, cmd)
			continue
		}
		if route.BotID != tc.bot {
			t.Errorf("comment %q: /%s routes to %q, want %q", tc.comment, cmd, route.BotID, tc.bot)
		}
	}
}
