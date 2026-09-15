package secrets

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const accountA = "c7f30bce-7e8b-4bb7-8199-afc172f3d580"
const accountB = "ce6b5bc9-11a8-477b-bd39-9c934d8c65f4"
const organizationA = "0d05ce64-6b67-4368-909c-0d26f5a5870a"

type profileTransport func(*http.Request) (*http.Response, error)

func (f profileTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDiscoverAnthropicAccount(t *testing.T) {
	client := &http.Client{Transport: profileTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != anthropicProfileURL() || r.Header.Get("Authorization") != "Bearer opaque-token" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Fatal("profile request did not use the fixed provider endpoint and headers")
		}
		if _, bounded := r.Context().Deadline(); !bounded {
			t.Fatal("profile lookup has no deadline")
		}
		body := fmt.Sprintf(`{"account":{"uuid":%q,"email":"person@example.org","extra":true},"organization":{"uuid":%q}}`, accountA, organizationA)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	a, err := DiscoverAnthropicAccount(t.Context(), client, "opaque-token")
	if err != nil || a.ID != accountA || a.OrganizationID != organizationA || a.Email != "person@example.org" || !IsAccountFingerprint(a.Fingerprint()) {
		t.Fatalf("account=%+v, error=%v", a, err)
	}
	// Email is display metadata. Token rotation, an email rename and the
	// Iterion owner/rank cannot create a second meter for this provider seat.
	same := a
	same.Email = "renamed@example.org"
	if same.Fingerprint() != a.Fingerprint() {
		t.Fatal("email rename changed subscription identity")
	}
	for _, other := range []OAuthAccount{
		{ID: accountB, OrganizationID: organizationA},
		{ID: accountA, OrganizationID: accountB},
	} {
		if other.Fingerprint() == a.Fingerprint() {
			t.Fatal("distinct provider seat shares a fingerprint")
		}
	}
}

func TestAnthropicProfileFailureDoesNotExposeResponseOrToken(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"forbidden", "private-account-response", 403},
		{"redirect", "private-account-response", 302},
		{"malformed", "private-account-response", 200},
		{"missing identity", `{"account":{"email":"private-account-response"}}`, 200},
		{"oversized", strings.Repeat("private-account-response", 4096), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: profileTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{"https://elsewhere.invalid/profile"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			a, err := DiscoverAnthropicAccount(t.Context(), client, "opaque-token")
			if err == nil || a.Fingerprint() != "" || calls != 1 {
				t.Fatalf("account=%+v error=%v calls=%d", a, err, calls)
			}
			if strings.Contains(err.Error(), "private-account-response") || strings.Contains(err.Error(), "opaque-token") {
				t.Fatal("profile error exposes sensitive data")
			}
		})
	}
	if IsAccountFingerprint("account:anthropic:"+strings.Repeat("z", 64)) || IsAccountFingerprint("legacy-blob-fp") {
		t.Fatal("invalid account fingerprint accepted")
	}
}

func TestAnthropicProfileCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &http.Client{Transport: profileTransport(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	if _, err := DiscoverAnthropicAccount(ctx, client, "opaque-token"); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
}
