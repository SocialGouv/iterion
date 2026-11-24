package forge_test

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/forge/forgejo"
	"github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/forge/gitlab"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestForgeProviderOAuthRedirects(t *testing.T) {
	for _, provider := range []string{"github", "gitlab", "forgejo"} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(provider+"/"+http.StatusText(status), func(t *testing.T) {
				var calls atomic.Int32
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Location", "https://user:secret@example.invalid/canonical?code=synthetic-code#secret")
					w.WriteHeader(status)
				}))
				defer s.Close()
				var ex forge.OAuthExchanger
				switch provider {
				case "github":
					ex = &github.OAuthApp{HTTP: s.Client(), BaseURL: s.URL, ClientID: "client", ClientSecret: "synthetic-secret"}
				case "gitlab":
					ex = &gitlab.OAuthApp{HTTP: s.Client(), BaseURL: s.URL, ClientID: "client", ClientSecret: "synthetic-secret"}
				case "forgejo":
					ex = &forgejo.OAuthApp{HTTP: s.Client(), BaseURL: s.URL, ClientID: "client", ClientSecret: "synthetic-secret"}
				}
				_, err := ex.Exchange(context.Background(), "synthetic-code", "http://callback.invalid", "synthetic-verifier")
				var redir *forge.RedirectError
				if !errors.As(err, &redir) || redir.Code != status || redir.Method != "POST" || calls.Load() != 1 {
					t.Fatalf("typed=%+v err=%v calls=%d", redir, err, calls.Load())
				}
				for _, secret := range []string{"user:", "synthetic-code", "secret"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("leaked %q: %v", secret, err)
					}
				}
			})
		}
	}
}
func TestForgeManifestRedirectCannotEchoExchangeCode(t *testing.T) {
	code := "synthetic-live-manifest-code"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://canonical.example.invalid"+r.URL.EscapedPath())
		w.WriteHeader(307)
	}))
	defer s.Close()
	_, err := github.ConvertManifest(context.Background(), s.Client(), s.URL, code)
	var redir *forge.RedirectError
	if !errors.As(err, &redir) {
		t.Fatalf("expected refusal: %v", err)
	}
	t.Logf("redirect=%s", redir.Location)
	if strings.Contains(err.Error(), code) || strings.Contains(redir.Location, code) {
		t.Fatal("canonical redirect leaks live manifest credential through typed Location and Error")
	}
}
func TestForgeOAuthRedirectEchoedPath(t *testing.T) {
	for _, provider := range []string{"github", "gitlab", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.ParseForm()
				w.Header().Set("Location", "https://canonical.example.invalid/"+r.Form.Get("client_secret")+"/"+r.Form.Get("code"))
				w.WriteHeader(303)
			}))
			defer s.Close()
			var ex forge.OAuthExchanger
			switch provider {
			case "github":
				ex = &github.OAuthApp{HTTP: s.Client(), BaseURL: s.URL, ClientSecret: "synthetic-client-secret"}
			case "gitlab":
				ex = &gitlab.OAuthApp{HTTP: s.Client(), BaseURL: s.URL, ClientSecret: "synthetic-client-secret"}
			case "forgejo":
				ex = &forgejo.OAuthApp{HTTP: s.Client(), BaseURL: s.URL, ClientSecret: "synthetic-client-secret"}
			}
			_, err := ex.Exchange(context.Background(), "synthetic-live-code", "http://callback.invalid", "")
			var redir *forge.RedirectError
			if !errors.As(err, &redir) {
				t.Fatalf("missing typed error: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic-client-secret") || strings.Contains(redir.Location, "synthetic-live-code") {
				t.Fatalf("non-hook credential reflected into diagnostic: %v", err)
			}
		})
	}
}
