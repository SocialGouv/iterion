package github

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// githubReviewComment is the create-review inline comment shape (modern
// line/side addressing, not the legacy diff `position`).
type githubReviewComment struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
}

type githubReview struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// renderGitHubCommentBody appends the one-click ```suggestion fence when the
// comment carries a replacement. GitHub applies the suggestion to the whole
// anchored span (start_line..line), so multi-line replacements work as-is.
func renderGitHubCommentBody(c forge.ReviewComment) string {
	body := c.Body
	if strings.TrimSpace(c.Suggestion) != "" {
		body += "\n\n```suggestion\n" + strings.TrimRight(c.Suggestion, "\n") + "\n```"
	}
	return body
}

// CreatePullReview posts one review (event COMMENT — advisory, never a merge
// gate) with inline comments on the PR's head (RIGHT) side.
//
// GitHub's create-review call is all-or-nothing: a single unanchorable
// comment 422s the whole review. On a 422 the review is retried once with
// every inline comment folded into the summary body (Fallback "summary") so
// the findings still land visibly instead of vanishing.
//
// CommentsPosted is re-fetched from the created review's comment listing
// (Verified true); when that confirmation read fails the submitted count is
// reported with Verified false.
func (c *AdminClient) CreatePullReview(ctx context.Context, repo string, number int, in forge.NewReview) (forge.ReviewResult, error) {
	prPath := "/repos/" + repo + "/pulls/" + strconv.Itoa(number)

	comments := make([]githubReviewComment, 0, len(in.Comments))
	suggestions := 0
	for _, rc := range in.Comments {
		gc := githubReviewComment{Path: rc.Path, Body: renderGitHubCommentBody(rc), Line: rc.Line, Side: "RIGHT"}
		if rc.LineEnd > rc.Line {
			gc.StartLine = rc.Line
			gc.StartSide = "RIGHT"
			gc.Line = rc.LineEnd
		}
		if strings.TrimSpace(rc.Suggestion) != "" {
			suggestions++
		}
		comments = append(comments, gc)
	}

	payload := map[string]any{"body": in.Body, "event": "COMMENT"}
	if len(comments) > 0 {
		payload["comments"] = comments
	}
	var rev githubReview
	code, err := c.do(ctx, http.MethodPost, prPath+"/reviews", payload, &rev)
	if err != nil {
		return forge.ReviewResult{}, err
	}
	if code == http.StatusUnprocessableEntity && len(comments) > 0 {
		// Inline anchoring rejected (a finding's line is not in the PR
		// diff). Fold everything into the body and post summary-only.
		fold := map[string]any{
			"body":  in.Body + "\n\n" + forge.FoldCommentsMarkdown(in.Comments),
			"event": "COMMENT",
		}
		var frev githubReview
		fcode, ferr := c.do(ctx, http.MethodPost, prPath+"/reviews", fold, &frev)
		if ferr != nil {
			return forge.ReviewResult{}, ferr
		}
		if fcode/100 != 2 {
			return forge.ReviewResult{}, statusErr("create pull review (summary fallback)", fcode)
		}
		return forge.ReviewResult{URL: frev.HTMLURL, CommentsPosted: 0, SuggestionsPosted: 0, Verified: true, Fallback: "summary"}, nil
	}
	if code/100 != 2 {
		return forge.ReviewResult{}, statusErr("create pull review", code)
	}

	res := forge.ReviewResult{URL: rev.HTMLURL, CommentsPosted: len(comments), SuggestionsPosted: suggestions}
	// Confirm what the forge actually stored — the anti-façade re-fetch.
	var stored []struct {
		ID int64 `json:"id"`
	}
	vcode, verr := c.do(ctx, http.MethodGet, prPath+"/reviews/"+strconv.FormatInt(rev.ID, 10)+"/comments?per_page=100", nil, &stored)
	if verr == nil && vcode == http.StatusOK {
		res.CommentsPosted = len(stored)
		res.Verified = true
		if res.CommentsPosted < len(comments) {
			res.Fallback = "partial"
			// Suggestions can't be attributed per-comment on a partial
			// store; report none rather than an optimistic count.
			res.SuggestionsPosted = 0
		}
	}
	return res, nil
}

// CreatePullReview on an App connection delegates to a management-token
// AdminClient minted on demand — the token is always live, which is the
// whole point of publishing server-side instead of from a run workspace
// holding a frozen installation token.
func (a *AppClient) CreatePullReview(ctx context.Context, repo string, number int, in forge.NewReview) (forge.ReviewResult, error) {
	rest, err := a.rest(ctx)
	if err != nil {
		return forge.ReviewResult{}, err
	}
	return rest.CreatePullReview(ctx, repo, number, in)
}

// ListPRReviewComments on an App connection reads the thread under the
// pull_requests:read profile — the review-thread reply gate resolves its
// client through the covering connection, which is an App connection by
// default, so the fetch has to exist here or the lane is dead on the
// ordinary integration shape.
func (a *AppClient) ListPRReviewComments(ctx context.Context, repo string, number int) ([]forge.PRReviewComment, error) {
	c, err := a.scopedREST(ctx, PRReviewCommentsInstallationPermissions())
	if err != nil {
		return nil, err
	}
	return c.ListPRReviewComments(ctx, repo, number)
}

// prReviewCommentWire is the list shape of GET /repos/{repo}/pulls/{n}/comments.
type prReviewCommentWire struct {
	ID        int64  `json:"id"`
	InReplyTo int64  `json:"in_reply_to_id"`
	Body      string `json:"body"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ListPRReviewComments returns the PR's review-thread comments (the inline
// diff comments and their replies), in chronological order. Backs the
// reply-in-thread conversational gate: it needs the whole set to
// reconstruct one thread (id == root || in_reply_to_id == root) and decide
// whether the bot participates in it. Pagination is capped at the NEWEST
// pages (fetched newest-first, then reversed): the gate classifies the
// thread just replied to, which lives at the new end — a cap on the oldest
// pages would blind it exactly on the long-lived PRs that exceed it — and
// an unbounded walk on a pathological PR would stall the webhook hot path.
func (c *AdminClient) ListPRReviewComments(ctx context.Context, repo string, number int) ([]forge.PRReviewComment, error) {
	const perPage = 100
	const maxPages = 5
	out := make([]forge.PRReviewComment, 0, perPage)
	for page := 1; page <= maxPages; page++ {
		var resp []prReviewCommentWire
		path := "/repos/" + repo + "/pulls/" + strconv.Itoa(number) + "/comments?sort=created&direction=desc&per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		code, err := c.do(ctx, http.MethodGet, path, nil, &resp)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, statusErr("GET pull review comments", code)
		}
		for _, w := range resp {
			out = append(out, forge.PRReviewComment{
				ID:        w.ID,
				InReplyTo: w.InReplyTo,
				Body:      w.Body,
				Path:      w.Path,
				CreatedAt: w.CreatedAt,
				Author:    w.User.Login,
			})
		}
		if len(resp) < perPage {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
