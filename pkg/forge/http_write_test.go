package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestForgeWriteRedirectMatrix(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, code := range []int{301, 302, 303, 307, 308} {
			for _, location := range []string{"/canonical", "", ":bad%location"} {
				t.Run(fmt.Sprintf("%s/%d/%s", method, code, location), func(t *testing.T) {
					var followed atomic.Int32
					s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/canonical" {
							followed.Add(1)
							w.WriteHeader(200)
							return
						}
						w.Header().Set("Location", location)
						w.WriteHeader(code)
					}))
					defer s.Close()
					_, err := DoJSON(context.Background(), nil, method, s.URL, "", nil, map[string]string{"secret": "value"}, nil)
					var redirect *RedirectError
					if !errors.As(err, &redirect) || redirect.Code != code || redirect.Method != method {
						t.Fatalf("write redirect not typed with original status: %v", err)
					}
					if followed.Load() != 0 {
						t.Fatal("redirected write reached canonical endpoint")
					}
					if forgeRejectedTheWrite(err) {
						t.Fatal("redirect incorrectly proves original write unapplied")
					}
				})
			}
		}
	}
}

func TestForgeReadRedirectPolicyAndTypedSecretScrub(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/end" {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Location", "/end")
		w.WriteHeader(303)
	}))
	defer s.Close()
	client := s.Client()
	for _, method := range []string{"GET", "HEAD"} {
		req, _ := http.NewRequest(method, s.URL, nil)
		resp, err := DoHTTP(client, req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("%s read redirect policy changed", method)
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest("GET", s.URL, nil)
	resp, err := DoHTTP(client, req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatal("injected read policy ignored")
	}
	secret := "iwh_secret"
	raw := &RedirectError{Method: "PUT", Code: 303, Location: safeRedirectLocation("https://user:password@example.test/iwh_%73ecret?token=hidden#fragment")}
	err = scrubHookSecret(raw, secret)
	var typed *RedirectError
	if !errors.As(err, &typed) {
		t.Fatal("typed redirect lost")
	}
	for _, got := range []string{err.Error(), typed.Location} {
		for _, forbidden := range []string{secret, "password", "hidden", "fragment"} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("redirect leaks %q: %s", forbidden, got)
			}
		}
	}
}
