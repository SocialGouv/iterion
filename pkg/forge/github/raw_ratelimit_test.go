package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The GitHub calls made outside the shared transport read a rate limit its
// way: the install callback's ownership check and the manifest conversion
// return the typed wait, naming when it ends, instead of a bare "HTTP 429".
func TestRawGitHubCallsTypeARateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	calls := map[string]func() error{
		"userCanAccessInstallation": func() error {
			return userCanAccessInstallation(ctx, srv.Client(), srv.URL, "t", 99)
		},
		"ConvertManifest": func() error {
			_, err := ConvertManifest(ctx, srv.Client(), srv.URL, "code")
			return err
		},
	}
	for name, call := range calls {
		err := call()
		var se *forge.StatusError
		if !errors.As(err, &se) || !se.RateLimited() {
			t.Errorf("%s: err = %v, want a rate-limited *forge.StatusError", name, err)
			continue
		}
		if wait := time.Until(se.ResetAt); wait < 58*time.Second || wait > time.Minute {
			t.Errorf("%s: ResetAt in %v, want ~1m from Retry-After", name, wait)
		}
	}
}
