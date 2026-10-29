package boardmongo

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	launchstore "github.com/SocialGouv/iterion/pkg/store"
)

// In-package on purpose: applyPatch is the Mongo twin's whole patch
// application, and the conformance leg that drives it through a real Update
// needs a Mongo instance (ITERION_TEST_MONGO_URI, the CI mongo job). This row
// pins the same decision with no database, so a twin drifting from the FS
// store's write boundary reddens on every machine.
func TestApplyPatchDropsTheMintedGrantFromACardsBotArgs(t *testing.T) {
	const sentinel = "sentinel-forge-grant-2036-mongo-unit"
	iss := &native.Issue{ID: "native:44444444-4444-4444-8444-444444444444", Title: "a writer patches the grant in"}
	next := map[string]string{"loop_cap": "5", launchstore.ForgePublishTokenVar: sentinel, launchstore.ForgePublishURLVar: "https://attacker.example/publish"}

	res := applyPatch(iss, native.Patch{BotArgs: &next}, native.DefaultBoard())
	if res.err != nil {
		t.Fatalf("applyPatch: %v", res.err)
	}
	for _, name := range launchstore.ServerMintedLaunchVars {
		if got, ok := iss.BotArgs[name]; ok {
			t.Errorf("the patched card carries %s = %q", name, got)
		}
	}
	if iss.BotArgs["loop_cap"] != "5" {
		t.Errorf("the writer's own arg was lost: %v", iss.BotArgs)
	}
	if next[launchstore.ForgePublishTokenVar] != sentinel {
		t.Errorf("the caller's patch map was mutated: %v", next)
	}

	// An EMPTY value withdraws a grant instead of granting one, here as on
	// the FS twin.
	withdrawal := map[string]string{launchstore.ForgePublishTokenVar: ""}
	if res := applyPatch(iss, native.Patch{BotArgs: &withdrawal}, native.DefaultBoard()); res.err != nil {
		t.Fatalf("applyPatch withdrawal: %v", res.err)
	}
	got, ok := iss.BotArgs[launchstore.ForgePublishTokenVar]
	if !ok || got != "" {
		t.Errorf("the withdrawal was dropped: got %q, present=%v", got, ok)
	}
}
