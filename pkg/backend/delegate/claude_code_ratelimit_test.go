package delegate

import (
	"strings"
	"testing"
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
