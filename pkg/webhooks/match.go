package webhooks

import (
	"slices"
	"strings"
)

// MatchEvent is the canonical event-kind allowlist matcher used by
// every provider call site (gitlab/github/forgejo). When allowlist is
// non-empty it accepts kind iff the list contains kind or "*". When
// empty the provider's defaults take over — variadic so each call site
// stays explicit about the zero-config contract:
//
//   - gitlab: MatchEvent(list, kind, "merge_request", "note") — both
//     the auto-review (MR open/reopen) and the on-demand /revi note
//     trigger reach a zero-config webhook.
//   - github / forgejo: MatchEvent(list, kind, "pull_request") — the
//     only event V1 handles.
//
// Operators who want to gate one off list the other explicitly
// (e.g. ["merge_request"] disables /revi while keeping auto-review).
func MatchEvent(allowlist []string, kind string, defaults ...string) bool {
	if len(allowlist) == 0 {
		return slices.Contains(defaults, kind)
	}
	return slices.Contains(allowlist, kind) || slices.Contains(allowlist, "*")
}

// MatchProject is the canonical project-path allowlist matcher shared
// by every provider call site (gitlab/github/forgejo) and the generic
// JSON webhook in pkg/server. An empty allowlist allows every project
// in the tenant. Each entry supports:
//
//   - a bare "*" (match all),
//   - a trailing "/*" prefix wildcard ("group/*" matches "group/anything"
//     and "group/sub/repo"),
//   - otherwise an exact match.
func MatchProject(allowlist []string, projectPath string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, pat := range allowlist {
		if matchProjectPattern(pat, projectPath) {
			return true
		}
	}
	return false
}

// MatchAuthor is the canonical PR/MR author-login allowlist matcher used
// by every provider call site (github/gitlab/forgejo). An empty allowlist
// allows any author. Matching is case-insensitive and trims surrounding
// space, so a webhook scoped to ["dependabot[bot]", "renovate[bot]"] reacts
// to a dependency bot's PRs while ignoring human PRs on the same repo. A
// "*" entry matches all (explicit allow-all). An empty login never matches a
// non-empty allowlist (an author we couldn't identify is not on the list).
//
// A leading "*" is a suffix wildcard: "*renovate[bot]" matches both the
// hosted "renovate[bot]" and a self-hosted App identity like
// "acme-renovate[bot]". Self-hosting Renovate/Dependabot under an org App is
// the common way to make their PRs trigger CI, and it renames the bot — so a
// dependency-guard bot has to recognise the family, not one fixed login.
func MatchAuthor(allowlist []string, login string) bool {
	return matchCaseInsensitiveAllowlist(allowlist, login)
}

// MatchAuthorRule is MatchAuthor plus a denylist: a denied login never
// matches, even when the allowlist is empty (= open). Deny is how a bot that
// claims a set of authors EXCLUSIVELY (a dependency-PR guard) keeps a general
// reviewer off the PRs it owns, without the reviewer's manifest having to know
// that guard exists. See BotRule.
func MatchAuthorRule(allow, deny []string, login string) bool {
	if len(deny) > 0 && strings.TrimSpace(login) != "" && matchCaseInsensitiveAllowlist(deny, login) {
		return false
	}
	return matchCaseInsensitiveAllowlist(allow, login)
}

// MatchLabel reports whether a freshly-applied issue label triggers a
// launch under this webhook's LabelAllowlist. An empty allowlist means
// "any label triggers" (the operator gates by which events the forge hook
// subscribes to instead). Matching is case-insensitive and trims space, so
// ["implement"] reacts to a "Implement" / "implement" label. A "*" entry is
// an explicit allow-all; an empty applied label never matches a non-empty
// allowlist (an unlabeled/edited event carries no label to match).
func MatchLabel(allowlist []string, label string) bool {
	return matchCaseInsensitiveAllowlist(allowlist, label)
}

// matchCaseInsensitiveAllowlist is the shared body behind MatchAuthor and
// MatchLabel: empty allowlist matches all; otherwise a trimmed, case-
// insensitive match against value, with "*" as an explicit allow-all, a
// leading "*" as a suffix wildcard, and an empty value never matching a
// non-empty allowlist.
func matchCaseInsensitiveAllowlist(allowlist []string, value string) bool {
	if len(allowlist) == 0 {
		return true
	}
	value = strings.TrimSpace(value)
	for _, pat := range allowlist {
		pat = strings.TrimSpace(pat)
		if pat == "*" {
			return true
		}
		if value == "" {
			continue
		}
		if suffix, ok := strings.CutPrefix(pat, "*"); ok {
			if suffix != "" && len(value) >= len(suffix) &&
				strings.EqualFold(suffix, value[len(value)-len(suffix):]) {
				return true
			}
			continue
		}
		if strings.EqualFold(pat, value) {
			return true
		}
	}
	return false
}

// HeldByLabel returns the first label in `present` that appears in the
// bot-agnostic `holdLabels` suppression set (case-insensitive), or "" when
// none does. An empty holdLabels set (the default) always returns "" — the
// hold gate is opt-in, so unlike an allowlist an empty set matches NOTHING;
// past that guard it reuses the canonical matcher (so a `*` entry holds all).
func HeldByLabel(holdLabels, present []string) string {
	if len(holdLabels) == 0 {
		return ""
	}
	return FirstMatchingLabel(holdLabels, present)
}

// FirstMatchingLabel returns the first freshly-applied label that passes the
// allowlist — the trigger — or "" when none qualifies. With an empty allowlist
// any added label qualifies (the first is returned). Used by the GitLab issues
// path, where one event can add several labels at once (changes.labels diff).
func FirstMatchingLabel(allowlist, added []string) string {
	for _, l := range added {
		if MatchLabel(allowlist, strings.TrimSpace(l)) {
			return l
		}
	}
	return ""
}

func matchProjectPattern(pat, path string) bool {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return false
	}
	if pat == "*" {
		return true
	}
	if strings.HasSuffix(pat, "/*") {
		prefix := strings.TrimSuffix(pat, "*") // keeps the trailing slash
		return strings.HasPrefix(path, prefix)
	}
	return pat == path
}
