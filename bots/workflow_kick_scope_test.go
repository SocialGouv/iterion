package bots

import (
	"maps"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// workflow-kick asks for exactly what its one gesture needs: the actions
// scope at write, which mints the dispatch capability into its run's publish
// grant (#3130). Any other scope would be a capability or an OAuth widening
// nobody asked for — the bot reads no code, posts no review, writes nothing.
func TestWorkflowKickDeclaresTheActionsScopeAndNothingElse(t *testing.T) {
	m, err := bundle.LoadManifest("workflow-kick/manifest.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Forge == nil {
		t.Fatal("workflow-kick declares no forge: block — its run would hold no dispatch capability")
	}
	if want := map[string]string{bundle.ForgeScopeActions: "write"}; !maps.Equal(m.Forge.TokenScopes, want) {
		t.Fatalf("workflow-kick token_scopes = %v, want exactly %v", m.Forge.TokenScopes, want)
	}
	if len(m.Forge.Events) != 0 {
		t.Fatalf("workflow-kick subscribes to forge events %v — it is launched by its schedule alone", m.Forge.Events)
	}
}
