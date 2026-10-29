package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// pushTaxonomyBundle is a bundle whose SOURCES ask for no floor at all and
// whose manifest declares the catalogue taxonomy keys: what asks for the
// release is the manifest, not the workflow.
func pushTaxonomyBundle(requires string) string {
	manifest := "name: grouped\nversion: 1.0.0\ncategory: verify\ntags: [read-only]\n"
	if requires != "" {
		manifest += "requires:\n  iterion: \"" + requires + "\"\n"
	}
	body, _ := json.Marshal(map[string]any{"files": map[string]string{
		botsource.MainBotFile: "workflow main:\n  entry: done\n",
		"manifest.yaml":       manifest,
	}})
	return string(body)
}

// The push admission is asked for a manifest-key floor exactly as it is
// asked for a syntax floor — one predicate, one subject: a bundle declaring
// `category`/`tags` below the release that reads them is refused with a 409
// naming the keys, the release and what a build below it does (it refuses
// the whole manifest at its strict decode), and the same push forced
// carries that sentence as its warning.
//
// Reddens on the mutation that drops the manifest half of the floor subject
// (bundle.FloorSubject.Keys), and on an early exit that asks only the
// sources (SyntaxRequirements.Asks) before reading the verdict.
func TestAdminBotsPush_RefusesAManifestKeyFloorBelowItsRelease(t *testing.T) {
	pinServerBuild(t, "v"+bundle.CatalogTaxonomySince+"+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v" + bundle.CatalogTaxonomySince + "+abc123"}}
	below := floortest.Below(t, bundle.CatalogTaxonomySince)

	w := adminBotsPutQuery(s, admin, "grouped", "", pushTaxonomyBundle(">= "+below))
	if w.Code != http.StatusConflict {
		t.Fatalf("push = %d %s, want 409 — a taxonomy floor below %s admits pods that refuse the manifest outright", w.Code, w.Body.String(), bundle.CatalogTaxonomySince)
	}
	for _, want := range []string{"`category`/`tags`", below, bundle.CatalogTaxonomySince} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("the refusal does not name %q: %s", want, w.Body.String())
		}
	}

	w = adminBotsPutQuery(s, admin, "grouped", "", pushTaxonomyBundle(">= "+bundle.CatalogTaxonomySince))
	if w.Code/100 != 2 {
		t.Fatalf("push with the taxonomy release declared = %d %s", w.Code, w.Body.String())
	}

	w = adminBotsPutQuery(s, admin, "grouped2", "force=1", pushTaxonomyBundle(""))
	if w.Code/100 != 2 || !strings.Contains(w.Body.String(), "FORCED past the syntax-floor guard") {
		t.Fatalf("forced push = %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "refuses the whole document") {
		t.Fatalf("the forced warning does not carry this floor's own consequence: %s", w.Body.String())
	}
}
