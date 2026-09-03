package forge

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestToNativeEvents(t *testing.T) {
	all := []string{bundle.ForgeEventPullRequest, bundle.ForgeEventPullRequestComment}
	cases := []struct {
		provider Provider
		in       []string
		want     []string
	}{
		{ProviderGitLab, all, []string{"merge_request", "note"}},
		// pull_request_comment does NOT include the review-thread firehose:
		// only a bot that declares pull_request_review_comment (the
		// conversational lane) makes a repo subscribe it.
		{ProviderGitHub, all, []string{"issue_comment", "pull_request"}},
		{ProviderForgejo, all, []string{"issue_comment", "pull_request"}},
		{ProviderGitHub, []string{bundle.ForgeEventPullRequestReviewComment}, []string{"pull_request_review_comment"}},
		// Forgejo's review-thread lane is not wired: no native subscription.
		{ProviderForgejo, []string{bundle.ForgeEventPullRequestReviewComment}, nil},
		// GitLab's single "note" carries both comment shapes — declaring the
		// pair still subscribes one native event.
		{ProviderGitLab, []string{bundle.ForgeEventPullRequestComment, bundle.ForgeEventPullRequestReviewComment}, []string{"note"}},
		{ProviderGitHub, []string{bundle.ForgeEventPullRequestComment, bundle.ForgeEventPullRequestReviewComment}, []string{"issue_comment", "pull_request_review_comment"}},
		{ProviderGitLab, []string{bundle.ForgeEventPullRequest}, []string{"merge_request"}},
		// issue_labeled → "issues" native event on every provider (gitlab's
		// admin client maps it to the issues_events boolean hook field).
		{ProviderGitHub, []string{bundle.ForgeEventIssueLabeled}, []string{"issues"}},
		{ProviderForgejo, []string{bundle.ForgeEventIssueLabeled}, []string{"issues"}},
		{ProviderGitLab, []string{bundle.ForgeEventIssueLabeled}, []string{"issues"}},
		{ProviderGitHub, []string{bundle.ForgeEventPullRequest, bundle.ForgeEventIssueLabeled}, []string{"issues", "pull_request"}},
		{ProviderGitLab, []string{"bogus"}, nil},
		{ProviderGitLab, []string{bundle.ForgeEventPullRequest, bundle.ForgeEventPullRequest}, []string{"merge_request"}}, // dedup
	}
	for _, c := range cases {
		got := ToNativeEvents(c.provider, c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ToNativeEvents(%s, %v) = %v, want %v", c.provider, c.in, got, c.want)
		}
	}
}

func TestUnionEvents(t *testing.T) {
	a := &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventPullRequest}}
	b := &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventPullRequestComment, bundle.ForgeEventPullRequest}}
	got := UnionEvents(a, b, nil)
	want := []string{bundle.ForgeEventPullRequest, bundle.ForgeEventPullRequestComment}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UnionEvents = %v, want %v", got, want)
	}
}

func TestUnionScopes(t *testing.T) {
	a := &bundle.ForgeRequirements{TokenScopes: map[string]string{"repository": "read", "pull_requests": "read"}}
	b := &bundle.ForgeRequirements{TokenScopes: map[string]string{"pull_requests": "write"}}
	got := UnionScopes(a, b)
	if got["pull_requests"] != "write" { // write beats read
		t.Errorf("pull_requests = %q, want write", got["pull_requests"])
	}
	if got["repository"] != "read" {
		t.Errorf("repository = %q, want read", got["repository"])
	}
}
