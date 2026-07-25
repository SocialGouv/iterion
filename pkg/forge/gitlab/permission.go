package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// CollaboratorPermission returns user's effective permission on the project,
// mapped from GitLab access levels onto the GitHub role vocabulary
// (admin|maintain|write|triage|read|none) so one cross-forge rank scale
// serves every provider. Membership is resolved via
// GET /projects/{id}/members/all/{user_id} (direct + inherited); a user with
// no membership — or an unknown username — is ("none", nil), not an error.
func (c *AdminClient) CollaboratorPermission(ctx context.Context, repo, user string) (string, error) {
	var users []struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	code, err := c.do(ctx, http.MethodGet, "/users?username="+url.QueryEscape(user), nil, &users)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", statusErr("GET /users?username", code)
	}
	var userID int64
	for _, u := range users {
		// The username filter is an exact match server-side, but guard anyway.
		if u.Username == user {
			userID = u.ID
			break
		}
	}
	if userID == 0 {
		return "none", nil
	}

	var m struct {
		AccessLevel int `json:"access_level"`
	}
	code, err = c.do(ctx, http.MethodGet, "/projects/"+projectID(repo)+"/members/all/"+strconv.FormatInt(userID, 10), nil, &m)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "none", nil
	}
	if code != http.StatusOK {
		return "", statusErr("GET project member", code)
	}
	switch {
	case m.AccessLevel >= 50:
		return "admin", nil
	case m.AccessLevel >= 40:
		return "maintain", nil
	case m.AccessLevel >= 30:
		return "write", nil
	case m.AccessLevel >= 20:
		return "triage", nil
	case m.AccessLevel >= 10:
		return "read", nil
	}
	return "none", nil
}

var _ forge.PermissionClient = (*AdminClient)(nil)
