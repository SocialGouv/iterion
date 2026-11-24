package dryrun

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2827: the dry run is the preview a template naming a server-minted secret
// var reaches — it renders with no guard behind it, and the placeholder
// indirection exists only where the materialiser that resolves it exists. A
// launch value holding the publish grant must come back as the guard's
// placeholder in every body the preview prepares, never as the value.
const mintedPreviewBot = `tool publish:
  command: "iterion-publish --token {{vars.forge_publish_token}}"
  postcondition: "test -n {{vars.forge_publish_token}}"

tool scripted:
  language: sh
  script: "printf %s {{vars.forge_publish_token}}"

workflow mintedpreview:
  worktree: none
  sandbox: none
  vars:
    forge_publish_token: string = ""
  entry: publish
  publish -> scripted
  scripted -> done
`

func TestAPreviewNeverRendersAMintedGrant(t *testing.T) {
	tok := "minted-preview-grant-3f9a7c2e5b1d4806"
	rec := &bodyRecorder{}
	wf := compileBot(t, mintedPreviewBot)
	if _, err := Run(context.Background(), wf, Options{
		Shell:  rec,
		Inputs: map[string]any{store.ForgePublishTokenVar: tok},
	}); err != nil {
		t.Fatal(err)
	}
	bodies := rec.bodies()
	if len(bodies) == 0 {
		t.Fatal("the preview prepared no body at all: the witness would pass on nothing")
	}
	ph := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	for _, body := range bodies {
		if strings.Contains(body, tok) {
			t.Errorf("a preview body carries the grant in clear: %q", body)
		}
		if !strings.Contains(body, ph) {
			t.Errorf("a preview body naming the grant shows no placeholder: %q", body)
		}
	}
}
