package forgejo

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// forgejoReviewComment is the create-review inline comment shape. Anchoring
// is single-line (new_position); multi-line spans anchor at their start.
type forgejoReviewComment struct {
	Path        string `json:"path"`
	Body        string `json:"body"`
	NewPosition int    `json:"new_position"`
}

type forgejoReview struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// renderForgejoCommentBody appends a ```suggestion fence for single-line
// spans (Forgejo/Gitea applies a suggestion to the one commented line); a
// multi-line replacement is rendered as a plain fenced block instead of a
// one-click suggestion that would silently apply to the wrong range.
func renderForgejoCommentBody(c forge.ReviewComment) string {
	body := c.Body
	sugg := strings.TrimRight(c.Suggestion, "\n")
	if strings.TrimSpace(sugg) == "" {
		return body
	}
	if c.LineEnd > c.Line {
		return body + "\n\nProposed replacement for lines " + strconv.Itoa(c.Line) + "-" + strconv.Itoa(c.LineEnd) + ":\n```\n" + sugg + "\n```"
	}
	return body + "\n\n```suggestion\n" + sugg + "\n```"
}

// CreatePullReview posts one review (event COMMENT — advisory) with inline
// comments. Like GitHub, the create call is all-or-nothing: on a 422 the
// review is retried once summary-only with the comments folded into the
// body (Fallback "summary"). CommentsPosted is re-fetched from the created
// review's comment listing when possible (Verified true).
func (c *AdminClient) CreatePullReview(ctx context.Context, repo string, number int, in forge.NewReview) (forge.ReviewResult, error) {
	prPath := "/repos/" + repo + "/pulls/" + strconv.Itoa(number)

	comments := make([]forgejoReviewComment, 0, len(in.Comments))
	suggestions := 0
	for _, rc := range in.Comments {
		comments = append(comments, forgejoReviewComment{Path: rc.Path, Body: renderForgejoCommentBody(rc), NewPosition: rc.Line})
		if strings.TrimSpace(rc.Suggestion) != "" && rc.LineEnd <= rc.Line {
			suggestions++
		}
	}

	payload := map[string]any{"body": in.Body, "event": "COMMENT"}
	if len(comments) > 0 {
		payload["comments"] = comments
	}
	var rev forgejoReview
	code, err := c.do(ctx, http.MethodPost, prPath+"/reviews", payload, &rev)
	if err != nil {
		return forge.ReviewResult{}, err
	}
	if code == http.StatusUnprocessableEntity && len(comments) > 0 {
		fold := map[string]any{
			"body":  in.Body + "\n\n" + forge.FoldCommentsMarkdown(in.Comments),
			"event": "COMMENT",
		}
		var frev forgejoReview
		fcode, ferr := c.do(ctx, http.MethodPost, prPath+"/reviews", fold, &frev)
		if ferr != nil {
			return forge.ReviewResult{}, ferr
		}
		if fcode/100 != 2 {
			return forge.ReviewResult{}, statusErr("create pull review (summary fallback)", fcode)
		}
		return forge.ReviewResult{URL: frev.HTMLURL, Verified: true, Fallback: "summary"}, nil
	}
	if code/100 != 2 {
		return forge.ReviewResult{}, statusErr("create pull review", code)
	}

	res := forge.ReviewResult{URL: rev.HTMLURL, CommentsPosted: len(comments), SuggestionsPosted: suggestions}
	var stored []struct {
		ID int64 `json:"id"`
	}
	vcode, verr := c.do(ctx, http.MethodGet, prPath+"/reviews/"+strconv.FormatInt(rev.ID, 10)+"/comments", nil, &stored)
	if verr == nil && vcode == http.StatusOK {
		res.CommentsPosted = len(stored)
		res.Verified = true
		if res.CommentsPosted < len(comments) {
			res.Fallback = "partial"
			res.SuggestionsPosted = 0
		}
	}
	return res, nil
}
