package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// effectiveFindByName is the chokepoint every role derivation reads (the
// pause-notice role, the hand-off consumers, the bots route). The launcher
// accepts the normalised spellings of a bot id, so the lookup must resolve
// them too, after an exact pass — never a partial or fuzzy one.
func TestEffectiveFindByName_ResolvesNormalisedSpellings(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	cases := []struct {
		name   string
		wantOK bool
	}{
		{"review-pr", true},
		{"review_pr", true},
		{"Review PR", true},
		{"review", false},
		{"no-such-bot", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run("name="+c.name, func(t *testing.T) {
			e, ok, err := s.effectiveFindByName(c.name)
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.wantOK {
				t.Fatalf("effectiveFindByName(%q) ok=%v, want %v", c.name, ok, c.wantOK)
			}
			if ok && e.Name != "review-pr" {
				t.Fatalf("effectiveFindByName(%q) resolved %q, want review-pr", c.name, e.Name)
			}
		})
	}
}

// entryOrigin and botExists are PROBES over the same entry set
// effectiveFindByName resolves through, and the overlay route refuses on
// entryOrigin. A probe stricter than the resolution it gates is a bypass: a
// folded spelling would read as "catalog"/"absent" while every resolver hands
// back the platform entry, so `PUT /bots/review_pr/overlay` on a platform
// override would skip its 409 and answer 200 for a write that changes
// nothing.
func TestPlatformProbesShareTheResolverTolerance(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	ctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	if _, err := s.botSources.Create(ctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "review-pr",
		Files: map[string]string{
			botsource.MainBotFile: testBotMain,
			"manifest.yaml":       "name: review-pr\ndisplay_name: Revi\n",
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.invalidatePlatformBots()

	for _, spelling := range []string{"review-pr", "review_pr", "Review PR"} {
		t.Run("name="+spelling, func(t *testing.T) {
			if got := s.entryOrigin(spelling); got != "platform" {
				t.Errorf("entryOrigin(%q) = %q, want platform — the overlay route's 409 is dodged by spelling", spelling, got)
			}
			if !s.botExists(spelling) {
				t.Errorf("botExists(%q) = false while the resolver returns the platform entry", spelling)
			}
			e, ok, err := s.effectiveFindByName(spelling)
			if err != nil || !ok || e.Name != "review-pr" {
				t.Errorf("effectiveFindByName(%q) = (%q, %v, %v)", spelling, e.Name, ok, err)
			}
		})
	}
	// A name that is not a platform override still reads as catalog.
	if got := s.entryOrigin("no-such-bot"); got != "catalog" {
		t.Errorf("entryOrigin(no-such-bot) = %q, want catalog", got)
	}
}
