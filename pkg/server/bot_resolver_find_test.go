package server

import "testing"

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
