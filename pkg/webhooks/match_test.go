package webhooks

import "testing"

func TestMatchEvent_DefaultsAndAllowlist(t *testing.T) {
	// Default kinds — variadic mirrors each provider's zero-config
	// contract: gitlab defaults to {merge_request, note} (auto-review
	// AND /revi paths), github/forgejo default to {pull_request}.
	if !MatchEvent(nil, "merge_request", "merge_request", "note") ||
		!MatchEvent(nil, "note", "merge_request", "note") ||
		MatchEvent(nil, "push", "merge_request", "note") {
		t.Fatal("gitlab default allowlist should be {merge_request, note}")
	}
	if !MatchEvent(nil, "pull_request", "pull_request") || MatchEvent(nil, "push", "pull_request") {
		t.Fatal("github/forgejo default allowlist should be {pull_request}")
	}
	if MatchEvent(nil, "anything") {
		t.Fatal("empty allowlist + no defaults must deny")
	}

	// Explicit allowlist supersedes defaults entirely.
	if !MatchEvent([]string{"*"}, "anything", "pull_request") {
		t.Fatal("wildcard event must match")
	}
	if !MatchEvent([]string{"push", "merge_request"}, "push", "merge_request", "note") {
		t.Fatal("explicit allow must match")
	}
	// Explicit allowlist excludes unlisted kinds even when they are in defaults.
	if MatchEvent([]string{"merge_request"}, "note", "merge_request", "note") {
		t.Fatal("explicit allowlist must exclude unlisted kinds (gates /revi off)")
	}
}

func TestMatchProject(t *testing.T) {
	if !MatchProject(nil, "acme/widgets") {
		t.Fatal("empty allowlist allows all")
	}
	if !MatchProject([]string{"acme/widgets"}, "acme/widgets") || MatchProject([]string{"acme/widgets"}, "acme/gadgets") {
		t.Fatal("exact match")
	}
	if !MatchProject([]string{"acme/*"}, "acme/anything") || !MatchProject([]string{"acme/*"}, "acme/sub/repo") {
		t.Fatal("prefix wildcard")
	}
	if MatchProject([]string{"acme/*"}, "other/repo") {
		t.Fatal("prefix wildcard should not cross group")
	}
	if !MatchProject([]string{"*"}, "any/thing") {
		t.Fatal("bare wildcard")
	}
}

func TestMatchAuthor(t *testing.T) {
	if !MatchAuthor(nil, "alice") || !MatchAuthor(nil, "") {
		t.Fatal("empty allowlist allows any author (including unknown)")
	}
	// Bot-login allowlist: the dependency-PR use case.
	deps := []string{"dependabot[bot]", "renovate[bot]"}
	if !MatchAuthor(deps, "dependabot[bot]") || !MatchAuthor(deps, "renovate[bot]") {
		t.Fatal("listed bot logins must match")
	}
	if MatchAuthor(deps, "alice") {
		t.Fatal("human author must be filtered out")
	}
	// Case-insensitive + space-tolerant (GitHub/GitLab casing drift).
	if !MatchAuthor([]string{"Dependabot[bot]"}, "dependabot[bot]") ||
		!MatchAuthor([]string{" renovate[bot] "}, "renovate[bot]") {
		t.Fatal("matching must be case-insensitive and trim space")
	}
	// An empty/unknown author never matches a non-empty allowlist.
	if MatchAuthor(deps, "") {
		t.Fatal("unknown author must not match a non-empty allowlist")
	}
	// Explicit wildcard opts back into allow-all.
	if !MatchAuthor([]string{"*"}, "alice") {
		t.Fatal("wildcard author must match")
	}
}

func TestMatchLabel(t *testing.T) {
	if !MatchLabel(nil, "implement") || !MatchLabel([]string{}, "anything") {
		t.Fatal("empty allowlist allows any label")
	}
	if !MatchLabel([]string{"implement"}, "implement") || MatchLabel([]string{"implement"}, "bug") {
		t.Fatal("exact match")
	}
	if !MatchLabel([]string{"implement"}, "Implement") {
		t.Fatal("matching must be case-insensitive")
	}
	if !MatchLabel([]string{"*"}, "whatever") {
		t.Fatal("bare wildcard matches any label")
	}
	if MatchLabel([]string{"implement"}, "") {
		t.Fatal("empty applied label must not match a non-empty allowlist")
	}
}

// A self-hosted Renovate/Dependabot runs under an org App, which renames the
// bot ("acme-renovate[bot]"), so a dependency-guard must match the family.
func TestMatchAuthorSuffixWildcard(t *testing.T) {
	list := []string{"dependabot[bot]", "*renovate[bot]"}
	for _, tc := range []struct {
		login string
		want  bool
	}{
		{"renovate[bot]", true},
		{"socialgouv-renovate[bot]", true},
		{"SocialGouv-Renovate[Bot]", true},
		{"dependabot[bot]", true},
		{"alice", false},
		{"renovate-helper", false},
		{"", false},
	} {
		if got := MatchAuthor(list, tc.login); got != tc.want {
			t.Errorf("MatchAuthor(%q) = %v, want %v", tc.login, got, tc.want)
		}
	}
	// A bare "*" stays an explicit allow-all, not a suffix match on "".
	if !MatchAuthor([]string{"*"}, "anyone") {
		t.Error(`"*" must still allow all`)
	}
}

func TestMatchAuthorRule(t *testing.T) {
	deny := []string{"*renovate[bot]"}
	for _, tc := range []struct {
		name        string
		allow, deny []string
		login       string
		want        bool
	}{
		{"no filter is open", nil, nil, "alice", true},
		{"deny beats an empty (open) allowlist", nil, deny, "renovate[bot]", false},
		{"deny beats an explicit allow", []string{"renovate[bot]"}, deny, "renovate[bot]", false},
		{"deny leaves others alone", nil, deny, "alice", true},
		{"allow still filters", []string{"alice"}, nil, "bob", false},
		// An author we could not identify must not be swallowed by a "*" deny:
		// that would silently black-hole every unattributed delivery.
		{"empty login vs wildcard deny", nil, []string{"*"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchAuthorRule(tc.allow, tc.deny, tc.login); got != tc.want {
				t.Errorf("MatchAuthorRule(%v, %v, %q) = %v, want %v", tc.allow, tc.deny, tc.login, got, tc.want)
			}
		})
	}
}
