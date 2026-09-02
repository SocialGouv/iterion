package forge

import (
	"sort"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// normalizedToNative maps an iterion-normalized event (the vocabulary a
// bot declares in its manifest forge.events block) to each provider's
// native webhook event name. GitLab's native names ("merge_request",
// "note") are translated again to boolean request-body fields inside the
// GitLab admin client; github/forgejo take the names verbatim.
//
// This same native vocabulary is what the inbound matchers + a
// webhooks.Config.EventAllowlist expect, so the orchestrator uses it for
// both the forge-side hook AND the iterion-side allowlist.
var normalizedToNative = map[string]map[Provider][]string{
	bundle.ForgeEventPullRequest: {
		ProviderGitLab:  {"merge_request"},
		ProviderGitHub:  {"pull_request"},
		ProviderForgejo: {"pull_request"},
	},
	// GitHub splits PR comments across TWO wire events: top-level PR
	// comments arrive as issue_comment, replies inside a review thread as
	// pull_request_review_comment. GitLab's single "note" covers both. The
	// normalized name means "comments on the PR", so the GitHub hook must
	// subscribe both — the review-thread half is what feeds the
	// reply-to-a-suggestion conversational lane. Forgejo stays on
	// issue_comment only until that lane is validated there.
	bundle.ForgeEventPullRequestComment: {
		ProviderGitLab:  {"note"},
		ProviderGitHub:  {"issue_comment", "pull_request_review_comment"},
		ProviderForgejo: {"issue_comment"},
	},
	// issue_labeled → the forge-native "issues" event. GitLab models it as a
	// boolean hook field (issues_events); the gitlab admin client translates
	// this native "issues" name to that field.
	bundle.ForgeEventIssueLabeled: {
		ProviderGitLab:  {"issues"},
		ProviderGitHub:  {"issues"},
		ProviderForgejo: {"issues"},
	},
}

// ToNativeEvents resolves a set of normalized event names to the given
// provider's native event names, de-duplicated and sorted (stable so a
// re-provision produces an identical hook spec → idempotent). Unknown
// normalized names are skipped (manifest validation already rejected them
// at parse time, so this is defensive).
func ToNativeEvents(p Provider, normalized []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ev := range normalized {
		m, ok := normalizedToNative[ev]
		if !ok {
			continue
		}
		for _, native := range m[p] {
			if seen[native] {
				continue
			}
			seen[native] = true
			out = append(out, native)
		}
	}
	sort.Strings(out)
	return out
}

// UnionEvents collects the normalized forge.events across several bots'
// requirements into one de-duplicated, sorted set — what the shared
// webhook for a repo must subscribe to when multiple bots are enabled.
func UnionEvents(reqs ...*bundle.ForgeRequirements) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range reqs {
		if r == nil {
			continue
		}
		for _, ev := range r.Events {
			if !seen[ev] {
				seen[ev] = true
				out = append(out, ev)
			}
		}
	}
	sort.Strings(out)
	return out
}

// UnionScopes merges the token_scopes maps across bots, keeping the
// HIGHEST level requested for each key (admin > write > read). The
// orchestrator/OAuth layer translates the union to the tightest provider
// scope that satisfies it.
func UnionScopes(reqs ...*bundle.ForgeRequirements) map[string]string {
	out := map[string]string{}
	for _, r := range reqs {
		if r == nil {
			continue
		}
		for k, v := range r.TokenScopes {
			if scopeRank(v) > scopeRank(out[k]) {
				out[k] = v
			}
		}
	}
	return out
}

func scopeRank(level string) int {
	switch level {
	case "admin":
		return 3
	case "write":
		return 2
	case "read":
		return 1
	}
	return 0
}
