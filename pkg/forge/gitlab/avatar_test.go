package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type avatarTransport func(*http.Request) (*http.Response, error)

func (f avatarTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCurrentAvatarPreservesImagesAndProvesMissingGravatar(t *testing.T) {
	const hash = "16220000000000000000000000000000"
	for _, tc := range []struct {
		name, avatar            string
		status                  int
		present, wantErr, probe bool
	}{
		{"empty", "", 0, false, false, false},
		{"uploaded", "https://gl.test/uploads/operator.png", 0, true, false, false},
		{"cdn", "https://cdn.test/avatar.png", 0, true, false, false},
		{"gravatar exists", "https://secure.gravatar.com/avatar/" + hash + "?d=identicon", 200, true, false, true},
		{"gravatar absent", "https://www.gravatar.com/avatar/" + hash + "?d=identicon&f=y&r=g", 404, false, false, true},
		{"gravatar unavailable", "https://gravatar.com/avatar/" + hash, 503, true, true, true},
		{"redirect refused", "https://gravatar.com/avatar/" + hash, 302, true, true, true},
		{"lookalike host", "https://gravatar.com.evil.test/avatar/" + hash, 0, true, false, false},
		{"private host", "http://127.0.0.1/avatar/" + hash, 0, true, false, false},
		{"userinfo", "https://token@gravatar.com/avatar/" + hash, 0, true, false, false},
		{"invalid hash", "https://gravatar.com/avatar/not-a-hash", 0, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			client := &http.Client{Transport: avatarTransport(func(r *http.Request) (*http.Response, error) {
				code, body := 200, ""
				if r.URL.Host == "gl.test" {
					if r.URL.Path != "/api/v4/user" || r.Header.Get("Authorization") != "Bearer secret" {
						t.Fatalf("wrong forge request: %s", r.URL)
					}
					raw, _ := json.Marshal(map[string]any{"avatar_url": tc.avatar})
					body = string(raw)
				} else {
					probes++
					if r.Method != http.MethodHead || r.URL.String() != "https://secure.gravatar.com/avatar/"+hash+"?d=404&r=x" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Fatalf("unsafe probe: %s %s", r.Method, r.URL)
					}
					code = tc.status
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Location": []string{"http://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			_, present, err := New(client, "https://gl.test", "secret").CurrentAvatar(context.Background())
			if present != tc.present || (err != nil) != tc.wantErr || (probes > 0) != tc.probe || probes > 1 {
				t.Fatalf("present=%v err=%v probes=%d", present, err, probes)
			}
		})
	}
}

func TestCurrentAvatarUncertainResponseDoesNotAuthorizeUpload(t *testing.T) {
	for _, body := range []string{`{}`, `{"avatar_url":42}`, `not-json`} {
		client := &http.Client{Transport: avatarTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}
		_, present, err := New(client, "https://gl.test", "secret").CurrentAvatar(context.Background())
		if !present || err == nil {
			t.Fatalf("body=%s present=%v err=%v", body, present, err)
		}
	}
	client := &http.Client{Transport: avatarTransport(func(r *http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })}
	_, present, err := New(client, "https://gl.test", "secret").CurrentAvatar(context.Background())
	if !present || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("present=%v err=%v", present, err)
	}
}
