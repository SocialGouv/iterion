package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// gitlabMRDiffRefs is the subset of GET /merge_requests/:iid needed to
// anchor positioned discussions.
type gitlabMRDiffRefs struct {
	WebURL   string `json:"web_url"`
	DiffRefs struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

// renderGitLabCommentBody appends GitLab's suggestion fence. The
// ```suggestion:-0+N syntax extends the applied range N lines below the
// anchored line, which maps a Line..LineEnd span with new_line = Line.
func renderGitLabCommentBody(c forge.ReviewComment) string {
	body := c.Body
	if strings.TrimSpace(c.Suggestion) != "" {
		below := 0
		if c.LineEnd > c.Line {
			below = c.LineEnd - c.Line
		}
		body += "\n\n```suggestion:-0+" + strconv.Itoa(below) + "\n" + strings.TrimRight(c.Suggestion, "\n") + "\n```"
	}
	return body
}

// CreatePullReview publishes a review onto a merge request. GitLab has no
// bot-postable single "review" object, so the equivalent is: one positioned
// discussion per inline comment (each an independent API call — naturally
// partial-tolerant) plus one summary note carrying the review body and any
// comments whose anchors the forge rejected.
//
// CommentsPosted counts the per-discussion 2xx creates (Verified true — each
// count is a confirmed forge write, no optimistic estimate).
func (c *AdminClient) CreatePullReview(ctx context.Context, repo string, number int, in forge.NewReview) (forge.ReviewResult, error) {
	mrPath := "/projects/" + projectID(repo) + "/merge_requests/" + strconv.Itoa(number)

	var mr gitlabMRDiffRefs
	code, err := c.do(ctx, http.MethodGet, mrPath, nil, &mr)
	if err != nil {
		return forge.ReviewResult{}, err
	}
	if code != http.StatusOK {
		return forge.ReviewResult{}, statusErr("get merge request", code)
	}

	posted, suggestions := 0, 0
	var failed []forge.ReviewComment
	for _, rc := range in.Comments {
		body := map[string]any{
			"body": renderGitLabCommentBody(rc),
			"position": map[string]any{
				"position_type": "text",
				"base_sha":      mr.DiffRefs.BaseSHA,
				"head_sha":      mr.DiffRefs.HeadSHA,
				"start_sha":     mr.DiffRefs.StartSHA,
				"new_path":      rc.Path,
				"new_line":      rc.Line,
			},
		}
		dcode, derr := c.do(ctx, http.MethodPost, mrPath+"/discussions", body, &struct{}{})
		if derr != nil || dcode/100 != 2 {
			failed = append(failed, rc)
			continue
		}
		posted++
		if strings.TrimSpace(rc.Suggestion) != "" {
			suggestions++
		}
	}

	summary := in.Body
	if len(failed) > 0 {
		summary += "\n\n" + forge.FoldCommentsMarkdown(failed)
	}
	ncode, nerr := c.do(ctx, http.MethodPost, mrPath+"/notes", map[string]any{"body": summary}, &struct{}{})
	noteOK := nerr == nil && ncode/100 == 2
	if !noteOK && posted == 0 {
		// Nothing landed at all — surface the failure, never fake success.
		if nerr != nil {
			return forge.ReviewResult{}, fmt.Errorf("create merge request summary note: %w", nerr)
		}
		return forge.ReviewResult{}, statusErr("create merge request summary note", ncode)
	}

	res := forge.ReviewResult{URL: mr.WebURL, CommentsPosted: posted, SuggestionsPosted: suggestions, Verified: true}
	if len(failed) > 0 || !noteOK {
		res.Fallback = "partial"
		if posted == 0 {
			res.Fallback = "summary"
		}
	}
	return res, nil
}
