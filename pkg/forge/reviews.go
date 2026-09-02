package forge

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// PR review publishing — the server-side, deterministic counterpart of a bot
// posting review comments itself. A workflow's findings are posted through
// the team connection's LIVE client (installation tokens minted on demand),
// so no forge credential ever needs to sit in the run's workspace.
// ---------------------------------------------------------------------------

// ReviewComment is one inline review comment anchored to a diff line of the
// PR's head revision (the "new"/RIGHT side). LineEnd > Line marks a
// multi-line span; LineEnd <= Line means a single-line anchor at Line.
type ReviewComment struct {
	Path string
	Line int
	// LineEnd is the inclusive end of the anchored span. Zero or == Line →
	// single-line comment.
	LineEnd int
	// Body is the comment markdown (severity/title/detail — already
	// rendered by the caller; providers only append the suggestion fence).
	Body string
	// Suggestion, when non-empty, is the literal replacement text for the
	// anchored span, rendered as the provider's one-click ```suggestion
	// block (syntax differs per forge, so rendering happens provider-side).
	Suggestion string
}

// NewReview is the payload for CreatePullReview: one review with a summary
// body plus zero or more inline comments. Reviews are always posted as
// non-blocking comments (never approve / request-changes) — iterion bots
// advise, they do not gate the merge.
type NewReview struct {
	Body     string
	Comments []ReviewComment
}

// ReviewResult reports what actually landed on the forge. Counts are
// re-fetched/confirmed from the forge whenever the provider API allows it
// (Verified true); when the confirmation read fails after a successful
// create, Verified is false and the counts are the submitted ones.
type ReviewResult struct {
	// URL links to the created review / comment thread.
	URL string
	// CommentsPosted is the number of inline comments the forge accepted.
	CommentsPosted int
	// SuggestionsPosted is how many of those carried a one-click
	// suggestion block.
	SuggestionsPosted int
	// Verified reports whether CommentsPosted was confirmed by a follow-up
	// read (or by per-comment create responses), not assumed.
	Verified bool
	// Fallback is "" for a clean inline post, "summary" when inline
	// anchoring was rejected wholesale and every comment was folded into
	// the review body, "partial" when only some inline comments landed
	// (the rest folded into the summary).
	Fallback string
}

// ReviewClient is the optional PR-review capability: posting a review with
// inline comments onto an existing pull/merge request. Implemented by the
// github, gitlab and forgejo admin clients.
type ReviewClient interface {
	CreatePullReview(ctx context.Context, repo string, number int, in NewReview) (ReviewResult, error)
}

// ReviewerAssigner is the optional capability of adding the client's OWN
// identity to a pull/merge request's reviewer set. It is what makes the
// forge-native "Re-request review" button exist on a reviewed PR: the
// inbound webhook treats a re-request targeting that identity as an
// on-demand re-review. Implemented by the gitlab admin client only — the
// forge that needs it: GitLab reviewers are explicit sidebar assignments a
// posted note never creates. GitHub adds a review's author to the reviewer
// list by itself (the button appears organically for PAT/OAuth-account
// connections), and a GitHub App cannot be a PR reviewer at all (forge
// restriction) — both are deliberate non-implementations, not gaps.
// Must be idempotent (a reviewer already present is a no-op) and additive
// (never drop the humans already on the reviewer set).
type ReviewerAssigner interface {
	AddSelfAsPullReviewer(ctx context.Context, repo string, number int) error
}

// FoldCommentsMarkdown renders inline comments as a markdown list for
// inclusion in a review's summary body — the fallback when a forge rejects
// (some of) the inline anchors. Suggestions are kept as plain fenced code
// (an unanchored ```suggestion block is meaningless).
func FoldCommentsMarkdown(comments []ReviewComment) string {
	if len(comments) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Findings that could not be anchored inline\n")
	for _, c := range comments {
		anchor := c.Path + ":" + strconv.Itoa(c.Line)
		if c.LineEnd > c.Line {
			anchor += "-" + strconv.Itoa(c.LineEnd)
		}
		b.WriteString("\n---\n**`" + anchor + "`**\n\n" + c.Body + "\n")
		if strings.TrimSpace(c.Suggestion) != "" {
			b.WriteString("\nProposed replacement:\n```\n" + c.Suggestion + "\n```\n")
		}
	}
	return b.String()
}

// pullURLRe matches the trailing "<repo>/pull|pulls|merge_requests/<n>" part
// of a forge PR web URL. The lazy repo group stops before GitLab's "/-"
// separator, which the optional group then consumes.
var pullURLRe = regexp.MustCompile(`^/(.+?)(?:/-)?/(?:pull|pulls|merge_requests)/(\d+)(?:[/?#].*)?$`)

// ParsePullURL extracts the forge host, repo slug and PR/MR number from a
// pull-request web URL. Supported shapes:
//
//	https://github.com/owner/repo/pull/42
//	https://forge.example/owner/repo/pulls/42          (Forgejo/Gitea)
//	https://gitlab.example/group/sub/proj/-/merge_requests/42
func ParsePullURL(raw string) (host, repo string, number int, err error) {
	u, perr := url.Parse(strings.TrimSpace(raw))
	if perr != nil {
		return "", "", 0, fmt.Errorf("parse pull URL: %w", perr)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", 0, fmt.Errorf("parse pull URL %q: scheme must be http(s)", raw)
	}
	if u.Host == "" {
		return "", "", 0, fmt.Errorf("parse pull URL %q: missing host", raw)
	}
	m := pullURLRe.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", 0, fmt.Errorf("parse pull URL %q: not a recognised pull/merge-request path", raw)
	}
	n, aerr := strconv.Atoi(m[2])
	if aerr != nil || n <= 0 {
		return "", "", 0, fmt.Errorf("parse pull URL %q: invalid PR number", raw)
	}
	return u.Host, strings.Trim(m[1], "/"), n, nil
}
