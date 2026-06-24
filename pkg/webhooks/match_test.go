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
