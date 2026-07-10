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

// resolveAppBotCommitterIdentity resolves the committer identity for a
// GitHub-App installation token, which — unlike a user/PAT token — cannot read
// `GET /user` (403). The publisher threads the App's bot login (e.g.
// "iterion-forge-1234[bot]"); an installation token CAN read the public
// `GET /users/<login>` to get its numeric id, yielding the canonical
// `<id>+<login>@users.noreply.github.com` that links the commit to the App bot
// (not the neutral iterion-runner[bot] fallback). Returns ok=false on any
// failure so the caller keeps its fallback; best-effort, never fatal.
func resolveAppBotCommitterIdentity(ctx context.Context, repoURL, botLogin, token string) (name, email string, ok bool) {
	botLogin = strings.TrimSpace(botLogin)
	if botLogin == "" || strings.TrimSpace(token) == "" {
		return "", "", false
	}
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	// Only GitHub is resolved here (App connections are GitHub-only today).
	if !strings.EqualFold(u.Host, "github.com") && !strings.EqualFold(u.Host, "www.github.com") {
		return "", "", false
	}
	endpoint := "https://api.github.com/users/" + url.PathEscape(botLogin)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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
