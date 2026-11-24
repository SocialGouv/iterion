package forgetrigger_test

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/forgetrigger"
)

// digestFixture is the catalogue the digest tests hash: a bot that declares
// its label invocation, a board one, a label bot that speaks through its
// forge: block and borrows from its first command, and a bot that claims no
// issue event.
type digestFixture struct {
	bots  []string
	inv   map[string][]bundle.Invocation
	fr    map[string]*bundle.ForgeRequirements
	roles forgetrigger.RoleBots
}

func newDigestFixture() digestFixture {
	return digestFixture{
		bots: []string{"feature-dev", "nestor", "review-pr", "saboteur"},
		inv: map[string][]bundle.Invocation{
			"nestor": {{
				Kind:        bundle.InvocationKindForge,
				ArgsVar:     "request",
				ContextVars: map[string]string{"force": "true", "tone": "dry", "depth": "2"},
				Forge:       &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{"nestor"}},
			}},
			"saboteur": {{
				Kind:  bundle.InvocationKindForge,
				Mode:  bundle.ExecutionBoard,
				Forge: &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{"saboteur", "*-chaos"}},
			}},
			"feature-dev": {{
				Kind:    bundle.InvocationKindCommand,
				ArgsVar: "feature_prompt",
				Command: &bundle.InvocationCommand{Name: "feature", OpensMR: true},
			}},
			"review-pr": {{
				Kind:  bundle.InvocationKindForge,
				Forge: &bundle.InvocationForge{Event: bundle.ForgeEventPullRequest},
			}},
		},
		fr: map[string]*bundle.ForgeRequirements{
			"nestor": {Webhook: &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"model": "small", "lang": "fr", "audit": "on"}}},
			"feature-dev": {
				Events:  []string{bundle.ForgeEventIssueLabeled},
				Labels:  []string{"feature"},
				Webhook: &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"open_mr": "true"}},
			},
		},
		roles: forgetrigger.RoleBots{Implementer: "feature-dev", Reviewer: "review-pr"},
	}
}

func (f digestFixture) digest() string {
	return forgetrigger.ManifestDigest(f.bots, f.inv, f.fr, f.roles)
}

func TestPlan_DigestStableAndSensitive(t *testing.T) {
	base := newDigestFixture().digest()

	t.Run("it is 16 hexadecimal characters, and pinned", func(t *testing.T) {
		if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(base) {
			t.Fatalf("digest %q is not 16 lower-case hexadecimal characters", base)
		}
		// The canonical document of the fixture, spelled out: bots by name,
		// routes in manifest order, maps by key. A change of it makes every
		// row projected before read as stale, so it is made on purpose
		// (digestFormat), never by accident.
		const canonical = `{"format":1,"bots":[` +
			`{"id":"feature-dev","routes":[{"key":"legacy:issue_labeled","mode":"board","labels":["feature"],"args_var":"feature_prompt","opens_mr":true,"context_vars":{}}],"launch_vars":{"open_mr":"true"}},` +
			`{"id":"nestor","routes":[{"key":"forge:issue_labeled|a:|l:8c2f0c0ba8f27eed","mode":"direct","labels":["nestor"],"args_var":"request","opens_mr":true,"context_vars":{"depth":"2","force":"true","tone":"dry"}}],"launch_vars":{"audit":"on","lang":"fr","model":"small"}},` +
			`{"id":"review-pr","routes":[],"launch_vars":{}},` +
			`{"id":"saboteur","routes":[{"key":"forge:issue_labeled|a:|l:0c6b782debbc6d5f","mode":"board","labels":["saboteur","*-chaos"],"args_var":"feature_prompt","opens_mr":true,"context_vars":{}}],"launch_vars":{}}` +
			`],"roles":{"implementer":"feature-dev","reviewer":"review-pr"}}`
		sum := sha256.Sum256([]byte(canonical))
		if want := hex.EncodeToString(sum[:8]); base != want {
			t.Errorf("digest of the fixture = %s; the first 8 bytes of the SHA-256 of its canonical document are %s", base, want)
		}
		if base != "cabd759f89f906b6" {
			t.Errorf("digest of the fixture = %s, pinned cabd759f89f906b6", base)
		}
	})

	t.Run("the order of the bots and of the maps changes nothing", func(t *testing.T) {
		for _, order := range [][]string{
			{"saboteur", "review-pr", "nestor", "feature-dev"},
			{"nestor", "feature-dev", "saboteur", "review-pr"},
			{"review-pr", "feature-dev", "nestor", "saboteur", "nestor", "feature-dev"},
		} {
			f := newDigestFixture()
			f.bots = order
			if got := f.digest(); got != base {
				t.Errorf("bots listed as %q: digest %s, want %s", order, got, base)
			}
		}
		// Go draws a new iteration order for each walk of a map: a digest
		// that followed it would move from one call to the next.
		for range 64 {
			f := newDigestFixture()
			rebuilt := map[string][]bundle.Invocation{}
			for _, bot := range []string{"saboteur", "review-pr", "nestor", "feature-dev"} {
				rebuilt[bot] = f.inv[bot]
			}
			f.inv = rebuilt
			if got := f.digest(); got != base {
				t.Fatalf("digest %s on a rebuilt catalogue, want %s", got, base)
			}
		}
	})

	t.Run("an absent manifest and an empty one hash alike", func(t *testing.T) {
		f := newDigestFixture()
		// The fixture holds no forge: block for these two bots.
		f.fr["saboteur"] = &bundle.ForgeRequirements{}
		f.fr["review-pr"] = nil
		// A bot listed with no manifest at all claims nothing, like one
		// whose manifest claims no issue event.
		f.inv["review-pr"] = nil
		if got := f.digest(); got != base {
			t.Errorf("digest %s, want %s", got, base)
		}
	})

	t.Run("a mode spelled as the default hashes like one left out", func(t *testing.T) {
		f := newDigestFixture()
		f.inv["nestor"][0].Mode = bundle.ExecutionDirect
		if got := f.digest(); got != base {
			t.Errorf("digest %s with mode: direct spelled out, want %s", got, base)
		}
	})

	edits := []struct {
		name string
		edit func(f *digestFixture)
	}{
		{"a label added to a declared invocation", func(f *digestFixture) {
			f.inv["nestor"][0].Forge.Labels = []string{"nestor", "triage"}
		}},
		{"a label added to a forge: block", func(f *digestFixture) {
			f.fr["feature-dev"].Labels = []string{"feature", "bug"}
		}},
		{"the forge actions of a declared invocation", func(f *digestFixture) {
			f.inv["nestor"][0].Forge.Actions = []string{"labeled"}
		}},
		{"a context var edited", func(f *digestFixture) {
			f.inv["nestor"][0].ContextVars["force"] = "false"
		}},
		{"a context var added", func(f *digestFixture) {
			f.inv["saboteur"][0].ContextVars = map[string]string{"force": "true"}
		}},
		{"a manifest launch var edited", func(f *digestFixture) {
			f.fr["nestor"].Webhook.LaunchVars["model"] = "large"
		}},
		{"a manifest launch var added to a bot that had none", func(f *digestFixture) {
			f.fr["saboteur"] = &bundle.ForgeRequirements{Webhook: &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"chaos": "1"}}}
		}},
		{"the implementer role", func(f *digestFixture) { f.roles.Implementer = "app-dev" }},
		{"the reviewer role", func(f *digestFixture) { f.roles.Reviewer = "review-mr" }},
		{"the mode of an invocation", func(f *digestFixture) { f.inv["nestor"][0].Mode = bundle.ExecutionBoard }},
		{"the args var of an invocation", func(f *digestFixture) { f.inv["nestor"][0].ArgsVar = "ask" }},
		{"the args var of the first command a block borrows from", func(f *digestFixture) {
			f.inv["feature-dev"][0].ArgsVar = "task_text"
		}},
		{"the opens_mr of the first command a block borrows from", func(f *digestFixture) {
			f.inv["feature-dev"][0].Command.OpensMR = false
		}},
		{"a block that stops claiming issue_labeled", func(f *digestFixture) {
			f.fr["feature-dev"].Events = []string{bundle.ForgeEventPullRequest}
		}},
		{"a bot added, even one that claims no issue event", func(f *digestFixture) {
			f.bots = append(f.bots, "extra")
			f.inv["extra"] = []bundle.Invocation{{Kind: bundle.InvocationKindForge, Forge: &bundle.InvocationForge{Event: bundle.ForgeEventPullRequest}}}
		}},
		{"a bot removed", func(f *digestFixture) { f.bots = []string{"feature-dev", "nestor", "saboteur"} }},
		{"a bot renamed, its manifest unchanged", func(f *digestFixture) {
			f.bots = []string{"feature-dev", "nestor-2", "review-pr", "saboteur"}
			f.inv["nestor-2"], f.fr["nestor-2"] = f.inv["nestor"], f.fr["nestor"]
		}},
	}
	seen := map[string]string{base: "the fixture"}
	for _, tc := range edits {
		t.Run("it moves with "+tc.name, func(t *testing.T) {
			f := newDigestFixture()
			tc.edit(&f)
			got := f.digest()
			if other, taken := seen[got]; taken {
				t.Errorf("digest %s is also the digest of %s", got, other)
			}
			seen[got] = tc.name
		})
	}
}

func TestPlan_StampsTheManifestDigestAndIgnoresBotTriggers(t *testing.T) {
	f := newDigestFixture()
	input := func(triggers map[string]forge.BotTrigger) forge.SubscriptionInput {
		in := forge.SubscriptionInput{
			Integration: forge.RepoIntegration{ID: testIntegration, TenantID: testTenant, RepoFullName: testRepo, BotIDs: slices.Clone(f.bots), BotTriggers: triggers},
			Config:      botScope(f.bots...),
			Bots:        slices.Clone(f.bots),
			InvByBot:    f.inv,
			FrByBot:     f.fr,
		}
		return in
	}
	plain := forgetrigger.PlanIntegration(input(nil), f.roles, planInstant)
	if plain.Err != nil {
		t.Fatalf("plan refused: %v", plain.Err)
	}
	if want := f.digest(); plain.Report.Digest != want {
		t.Fatalf("the plan reports digest %s; ManifestDigest of its inputs is %s", plain.Report.Digest, want)
	}
	if len(plain.Rows) != 3 {
		t.Fatalf("want the three issue rows of the fixture, got %d", len(plain.Rows))
	}
	for _, row := range plain.Rows {
		if row.ProjectionDigest != plain.Report.Digest {
			t.Errorf("bot %s: row digest %s, want %s", row.BotID, row.ProjectionDigest, plain.Report.Digest)
		}
	}

	// The operator's per-bot state changes the rows and leaves the digest:
	// every write of it projects on the spot, so no reader has to detect it.
	steered := forgetrigger.PlanIntegration(input(map[string]forge.BotTrigger{
		"nestor":   {Labels: []string{"triage"}},
		"saboteur": {Enabled: boolPtr(false)},
	}), f.roles, planInstant)
	if steered.Err != nil {
		t.Fatalf("plan refused: %v", steered.Err)
	}
	if steered.Report.Digest != plain.Report.Digest {
		t.Errorf("bot_triggers moved the digest: %s, want %s", steered.Report.Digest, plain.Report.Digest)
	}
	if row := rowOf(t, steered, "nestor"); !slices.Equal(row.Match.AddedLabels, []string{"triage"}) {
		t.Fatalf("control: bot_triggers did not reach nestor's row: %q", row.Match.AddedLabels)
	}
}
