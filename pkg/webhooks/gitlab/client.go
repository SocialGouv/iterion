package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// GitLab numeric access levels.
const (
	AccessGuest      = 10
	AccessReporter   = 20
	AccessDeveloper  = 30
	AccessMaintainer = 40
	AccessOwner      = 50
)

// RoleLevel maps a role name to its GitLab access level. Unknown / empty
// defaults to Developer — the sensible "can talk to the bot" floor.
func RoleLevel(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "guest":
		return AccessGuest
	case "reporter":
		return AccessReporter
	case "maintainer":
		return AccessMaintainer
	case "owner":
		return AccessOwner
	case "developer", "":
		return AccessDeveloper
	default:
		return AccessDeveloper
	}
}

// API is a minimal GitLab REST client for the conversational auth checks
// (loop-guard + role-gate). BaseURL is "https://<host>" (no /api/v4); auth
// is a PRIVATE-TOKEN header carrying the bot's forge_token.
type API struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
}

func (a API) get(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.BaseURL, "/")+"/api/v4"+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("PRIVATE-TOKEN", a.Token)
	c := a.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// CurrentUser returns the account the token authenticates as — used to skip
// the bot's own notes (loop-guard).
func (a API) CurrentUser(ctx context.Context) (User, error) {
	var u User
	code, err := a.get(ctx, "/user", &u)
	if err != nil {
		return User{}, err
	}
	if code != http.StatusOK {
		return User{}, fmt.Errorf("gitlab: GET /user: HTTP %d", code)
	}
	return u, nil
}

// MemberAccessLevel returns the user's effective access level on the project
// (inherited memberships included) and whether they are a member at all.
func (a API) MemberAccessLevel(ctx context.Context, projectID, userID int64) (level int, member bool, err error) {
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	code, err := a.get(ctx, "/projects/"+strconv.FormatInt(projectID, 10)+"/members/all/"+strconv.FormatInt(userID, 10), &m)
	if err != nil {
		return 0, false, err
	}
	switch code {
	case http.StatusOK:
		return m.AccessLevel, true, nil
	case http.StatusNotFound:
		return 0, false, nil // not a member
	default:
		return 0, false, fmt.Errorf("gitlab: member check: HTTP %d", code)
	}
}

// DiscussionNote is one note of an MR discussion thread — just the fields
// the conversational layer needs: authorship for the reply-in-thread
// classification (NotesHaveAuthor) and body for the thread transcript the
// converse bot receives (FormatThreadTranscript).
type DiscussionNote struct {
	AuthorID       int64
	AuthorUsername string
	Body           string
	System         bool
}

// Discussion fetches the notes of one MR discussion thread, in API order
// (chronological). An empty discussionID or a 404 returns (nil, nil) —
// callers treat "no thread" as benign.
func (a API) Discussion(ctx context.Context, projectID, mrIID int64, discussionID string) ([]DiscussionNote, error) {
	if discussionID == "" {
		return nil, nil
	}
	var d struct {
		Notes []struct {
			Author struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"author"`
			Body   string `json:"body"`
			System bool   `json:"system"`
		} `json:"notes"`
	}
	code, err := a.get(ctx, fmt.Sprintf("/projects/%d/merge_requests/%d/discussions/%s", projectID, mrIID, url.PathEscape(discussionID)), &d)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		notes := make([]DiscussionNote, 0, len(d.Notes))
		for _, n := range d.Notes {
			notes = append(notes, DiscussionNote{AuthorID: n.Author.ID, AuthorUsername: n.Author.Username, Body: n.Body, System: n.System})
		}
		return notes, nil
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("gitlab: get discussion: HTTP %d", code)
	}
}

// NotesHaveAuthor reports whether any note is authored by userID — the "is
// this a Revi thread" classification driving the reply-in-thread trigger
// (a plain reply in a thread the bot is part of is "talking to Revi").
func NotesHaveAuthor(notes []DiscussionNote, userID int64) bool {
	if userID == 0 {
		return false
	}
	for _, n := range notes {
		if n.AuthorID == userID {
			return true
		}
	}
	return false
}

// FormatThreadTranscript renders a discussion's notes as the plain-text
// transcript the converse bot receives as {{vars.thread_context}}:
// chronological, system notes skipped, the bot's own notes labelled so the
// model knows which earlier statements are its own. The budget policy
// (anchor kept, newest notes fill, omission marker) is the shared
// webhooks.CapTranscript, one policy for every forge's conversation lane.
func FormatThreadTranscript(notes []DiscussionNote, botUserID int64, maxChars int) string {
	rendered := make([]string, 0, len(notes))
	for _, n := range notes {
		if n.System {
			continue
		}
		who := "@" + n.AuthorUsername
		if botUserID != 0 && n.AuthorID == botUserID {
			who += " (you, the bot)"
		}
		rendered = append(rendered, who+":\n"+strings.TrimSpace(n.Body))
	}
	return webhooks.CapTranscript(rendered, maxChars)
}
