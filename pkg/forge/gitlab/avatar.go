package gitlab

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// CurrentAvatar preserves uploaded/external images, including a previously
// installed iterion avatar. GitLab's avatar_url can be a Gravatar fallback:
// d=identicon does not prove absence, since Gravatar serves a registered image
// first. Its documented d=404 response does prove absence. Unknown hosts and
// URL shapes are existing images; they are never fetched by this method.
func (c *AdminClient) CurrentAvatar(ctx context.Context) (string, bool, error) {
	var user struct {
		AvatarURL json.RawMessage `json:"avatar_url"`
	}
	code, err := c.do(ctx, http.MethodGet, "/user", nil, &user)
	if err != nil {
		return "", true, err
	}
	if code != http.StatusOK {
		return "", true, statusErr("GET /user avatar", code)
	}
	if len(user.AvatarURL) == 0 {
		return "", true, fmt.Errorf("gitlab: user response omitted avatar_url; preserving current avatar")
	}
	var avatarURL string
	if err := json.Unmarshal(user.AvatarURL, &avatarURL); err != nil {
		return "", true, fmt.Errorf("gitlab: read avatar_url: %w", err)
	}
	if avatarURL == "" {
		return "", false, nil
	}
	hash := gravatarHash(avatarURL)
	if hash == "" {
		return avatarURL, true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Build a fixed public URL from validated hex, with no forge credential,
	// cookies, source query, forced-default or redirects. r=x detects any rating.
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://secure.gravatar.com/avatar/"+hash+"?d=404&r=x", nil)
	if err != nil {
		return avatarURL, true, err
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := *httpClient
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return avatarURL, true, fmt.Errorf("check existing Gravatar: %w", err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotFound:
		return avatarURL, false, nil
	case http.StatusOK:
		return avatarURL, true, nil
	default:
		return avatarURL, true, fmt.Errorf("check existing Gravatar: HTTP %d; preserving current avatar", res.StatusCode)
	}
}

func gravatarHash(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Port() != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "gravatar.com", "www.gravatar.com", "secure.gravatar.com":
	default:
		return ""
	}
	if !strings.HasPrefix(u.Path, "/avatar/") {
		return ""
	}
	hash := strings.TrimPrefix(u.Path, "/avatar/")
	hash = strings.TrimSuffix(hash, ".jpg")
	if len(hash) != 32 && len(hash) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return ""
	}
	return strings.ToLower(hash)
}

var _ forge.AvatarReader = (*AdminClient)(nil)
