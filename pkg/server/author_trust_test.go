package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

type fakePermClient struct {
	perm  string
	err   error
	calls int
}

func (f *fakePermClient) CollaboratorPermission(_ context.Context, _, _ string) (string, error) {
	f.calls++
	return f.perm, f.err
}

func TestAuthorTrust(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		pc        *fakePermClient
		login     string
		assoc     string
		minRole   string
		allowlist []string
		want      bool
	}{
		{name: "empty login", pc: &fakePermClient{perm: "admin"}, login: "", want: false},
		{name: "allowlist wins without API", pc: nil, login: "viczei", allowlist: []string{"@viczei"}, want: true},
		{name: "assoc member fast path", pc: nil, login: "alice", assoc: "MEMBER", want: true},
		{name: "assoc owner fast path", pc: nil, login: "alice", assoc: "owner", want: true},
		{name: "assoc contributor not enough", pc: nil, login: "alice", assoc: "CONTRIBUTOR", want: false},
		{name: "assoc none no client fails closed", pc: nil, login: "drive-by", assoc: "NONE", want: false},
		{name: "api write >= default developer", pc: &fakePermClient{perm: "write"}, login: "bob", want: true},
		{name: "api read < default developer", pc: &fakePermClient{perm: "read"}, login: "bob", want: false},
		{name: "api triage >= reporter threshold", pc: &fakePermClient{perm: "triage"}, login: "bob", minRole: "reporter", want: true},
		{name: "api error fails closed", pc: &fakePermClient{err: errors.New("boom")}, login: "bob", want: false},
		{name: "dependency bot never trusted", pc: &fakePermClient{perm: "admin"}, login: "renovate[bot]", assoc: "MEMBER", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := newAuthorTrust()
			var pcArg = (*fakePermClient)(nil)
			if tc.pc != nil {
				pcArg = tc.pc
			}
			got := at.trusted(ctx, permClientOrNil(pcArg), "github", "acme/w", tc.login, tc.assoc, tc.minRole, tc.allowlist)
			if got != tc.want {
				t.Fatalf("trusted = %v, want %v", got, tc.want)
			}
		})
	}
}

// permClientOrNil converts a typed-nil fake into a true nil interface (the
// shape callers pass when the admin client lacks the capability).
func permClientOrNil(f *fakePermClient) forge.PermissionClient {
	if f == nil {
		return nil
	}
	return f
}

func TestAuthorTrustCacheTTL(t *testing.T) {
	ctx := context.Background()
	at := newAuthorTrust()
	pc := &fakePermClient{perm: "write"}

	for i := 0; i < 3; i++ {
		if !at.trusted(ctx, pc, "github", "acme/w", "bob", "", "", nil) {
			t.Fatalf("call %d: want trusted", i)
		}
	}
	if pc.calls != 1 {
		t.Fatalf("API called %d times, want 1 (cached)", pc.calls)
	}

	// Expire the entry — the next call re-probes.
	at.mu.Lock()
	for k, e := range at.cache {
		e.expires = time.Now().Add(-time.Second)
		at.cache[k] = e
	}
	at.mu.Unlock()
	if !at.trusted(ctx, pc, "github", "acme/w", "bob", "", "", nil) {
		t.Fatalf("post-expiry: want trusted")
	}
	if pc.calls != 2 {
		t.Fatalf("API called %d times after expiry, want 2", pc.calls)
	}
}
