package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// resolveForgeCommitterIdentity resolves the git author/committer identity that
// SHOULD sign a run's commits from the forge token that will push them, so a
// pushed commit is attributed to whoever actually owns that token — the
// connection's user (e.g. devthejo) today, the GitHub-App bot once the App is
// used — NOT to a stray account that happens to share the seeded fallback
// email. Returns ok=false on any failure so the caller keeps its fallback; the
// call is best-effort observability plumbing, never fatal.
//
// GitHub: GET /user with the token → {login, id}; the canonical noreply email
// `<id>+<login>@users.noreply.github.com` links the commit to that exact
// account (and only that account — the numeric id defeats username squatting).
// A GitHub-App INSTALLATION token can't read /user (403) → ok=false; App-bot
// attribution is wired separately when the App path lands.
func resolveForgeCommitterIdentity(ctx context.Context, repoURL, token string) (name, email string, ok bool) {
	if strings.TrimSpace(token) == "" {
		return "", "", false
	}
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	// Only GitHub is resolved here (the active forge). Other hosts fall back to
	// the caller's default until their /user shape is wired.
	if !strings.EqualFold(u.Host, "github.com") && !strings.EqualFold(u.Host, "www.github.com") {
		return "", "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", "", false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", "", false
	}
	var out struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Login == "" || out.ID == 0 {
		return "", "", false
	}
	return out.Login, fmt.Sprintf("%d+%s@users.noreply.github.com", out.ID, out.Login), true
}
