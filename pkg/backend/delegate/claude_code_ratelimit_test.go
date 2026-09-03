package delegate

import (
	"strings"
	"testing"
	"time"
)

func TestIsRateLimitMessage(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "anthropic forfait quota exhausted (real-world)",
			text: "You've hit your limit · resets May 12, 9pm (UTC)",
			want: true,
		},
		{
			name: "lowercase variant",
			text: "you've hit your limit. please try again later.",
			want: true,
		},
		{
			name: "anthropic forfait SESSION limit (real-world, run 019f2247)",
			text: "You've hit your session limit · resets 10:30am (UTC)",
			want: true,
		},
		{
			name: "anthropic forfait WEEKLY limit (real-world, feed-watch runner 019f7eee)",
			text: "You've hit your weekly limit · resets 9pm (Europe/Paris)",
			want: true,
		},
		{
			// The shape that re-opened the masking bug on 2026-09-03:
			// THREE words and an apostrophe between "your" and "limit".
			name: "org spend ceiling (real-world, run 01a06694)",
			text: "You've hit your org's monthly spend limit · ask your admin to raise it at claude.ai/settings/usage",
			want: true,
		},
		{
			// The qualifier is bounded at three words on purpose: an
			// agent narrating its own budget reasoning must not be read
			// as the provider cutting us off.
			name: "agent prose about limits is NOT a refusal",
			text: "I checked whether we hit your project's documented per-user monthly request ceiling limit and we did not.",
			want: false,
		},
		{
			name: "future noun variant (daily) — tolerant match keeps this from re-masking",
			text: "You've hit your daily limit · resets midnight (UTC)",
			want: true,
		},
		{
			name: "bare rate_limit_error substring NOT matched — left to SDK error path",
			text: "Error: rate_limit_error: too many requests",
			want: false,
		},
		{
			name: "generic quota exceeded relay",
			text: "Your monthly quota exceeded for this organization.",
			want: true,
		},
		{
			name: "generic rate limit exceeded relay",
			text: "Rate limit exceeded. Please retry in 30 seconds.",
			want: true,
		},
		{
			name: "zai (anthropic-shaped facade) 429 relay (real-world)",
			text: "API Error: Request rejected (429) · Usage limit reached for 5 hour. Your limit will reset at 2026-05-13 07:38:08",
			want: true,
		},
		{
			name: "usage limit reached alone",
			text: "Usage limit reached. Try again later.",
			want: true,
		},
		{
			name: "case-insensitive (429) match",
			text: "API ERROR: REQUEST REJECTED (429)",
			want: true,
		},
		{
			name: "empty text never matches",
			text: "",
			want: false,
		},
		{
			name: "agent prose about rate-limit CVE — must not match",
			text: "The package implements a token-bucket rate limiter to mitigate API abuse. Security audit confirms no rate_limit_error exposure beyond standard 429 handling.",
			want: false,
		},
		{
			name: "long agent reasoning that mentions hit your limit incidentally — guarded by length cap",
			text: strings.Repeat("The package documentation explains how to hit your limit and recover. ", 30),
			want: false,
		},
		{
			name: "real JSON output mentioning rate-limit in raw field",
			text: `{"safe":true,"cves":["CVE-2024-0001"],"raw":"npm audit reports no rate limit issues for this package version"}`,
			want: false,
		},
		{
			name: "unrelated short text never matches",
			text: "I'll inspect the repository to determine the package manager.",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRateLimitMessage(tc.text)
			if got != tc.want {
				t.Errorf("isRateLimitMessage(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestErrRateLimited_Error(t *testing.T) {
	e := &ErrRateLimited{Provider: BackendClaudeCode, Detail: "You've hit your limit"}
	got := e.Error()
	if !strings.Contains(got, "rate_limited") || !strings.Contains(got, BackendClaudeCode) {
		t.Errorf("Error() = %q, want it to contain rate_limited + provider name", got)
	}
}

// The codex bare-notice detector had the same masking gap the claude_code
// regex was widened for: its prefixes carry no qualifier, so every
// inserted-noun variant sailed through as a normal result. The opener
// anchor keeps the prefix discipline that stops agent prose qualifying.
func TestIsCodexBareLimitNoticeCoversNounVariants(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"You've hit your usage limit.", true},              // enumerated prefix, unchanged
		{"You've hit your weekly limit · resets 9pm", true}, // was missed
		{"You've hit your session limit · resets 10:30am", true},
		{"You've hit your org's monthly spend limit · ask your admin to raise it", true},
		{"I verified we never hit your weekly limit on this account.", false}, // prose, not an opener
		{"Selected model is at capacity.", true},
	}
	for _, c := range cases {
		if got := isCodexBareLimitNotice(c.text); got != c.want {
			t.Errorf("isCodexBareLimitNotice(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// A bare "spend limit" in the ACCEPTANCE gate would abort a node on
// ordinary agent narration — and worse, record a StatusRejected reading
// that routes every later run around that credential for an hour. The
// ceiling reaches the gate through its provider-shaped opener instead.
// Strings verified against this detector, not invented.
func TestSpendLimitProseIsNotARefusal(t *testing.T) {
	prose := []string{
		"I'll add a spend limit check to the config.",
		"The credpool docs say a metered pledge must carry a spend limit.",
		"Question: should the org spend limit be configurable per team?",
		"Now adding the spend limit classification to usagecap.go.",
	}
	for _, s := range prose {
		if isRateLimitMessage(s) {
			t.Errorf("agent prose must not read as a provider refusal: %q", s)
		}
	}
	// The provider's own notice still does, through "hit your … limit".
	real := "You've hit your org's monthly spend limit · ask your admin to raise it at claude.ai/settings/usage"
	if !isRateLimitMessage(real) {
		t.Fatalf("the provider notice must still be caught: %q", real)
	}
	kind, window, _ := classifyRateLimit(real, time.Now())
	if kind != RateLimitKindUsageWindow || string(window) != "spend" {
		t.Fatalf("kind=%q window=%q", kind, window)
	}
}
