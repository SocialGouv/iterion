package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// grantMintRecorder is a fake GitHub whose installation-token mint applies
// GitHub's rule — a request outside the installation's grant is refused
// (422 "permissions not granted") — and serves the one management call the
// tests make.
type grantMintRecorder struct {
	mu      sync.Mutex
	granted map[string]string // nil = accept every mint
	mints   []map[string]string
	srv     *httptest.Server
}

func newGrantMintRecorder(t *testing.T, granted map[string]string) *grantMintRecorder {
	t.Helper()
	r := &grantMintRecorder{granted: granted}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, "/access_tokens") {
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.granted != nil {
				for name, level := range body.Permissions {
					if !grantCovers(r.granted, name, level) {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_ = json.NewEncoder(w).Encode(map[string]any{"message": "The permissions requested are not granted to this installation."})
						return
					}
				}
			}
			r.mints = append(r.mints, body.Permissions)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_mgmt", "expires_at": "2099-01-01T00:00:00Z"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []map[string]any{{"full_name": "acme/widgets"}}})
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *grantMintRecorder) snapshot() []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]string(nil), r.mints...)
}

func (r *grantMintRecorder) appClient(t *testing.T, granted map[string]string) *AppClient {
	t.Helper()
	return &AppClient{
		HTTP: r.srv.Client(), WebBaseURL: r.srv.URL,
		Cfg: AppConfig{AppID: 42, PrivateKeyPEM: testKeyPEMOnce(t), AppSlug: "iterion"}, InstallationID: 99,
		Granted: granted,
	}
}

// The management token — what every connection lane writes through: publish,
// the gate reconcilers, /revi approve, hooks — is minted from the grant the
// connection recorded, not from the constant baseline. An installation whose
// owner approved LESS than the baseline used to 422 on every mint (and again
// on the statuses-less retry), so the connection could serve nothing, not
// even the calls its grant covers.
func TestAppClientManagementMintNarrowsToTheGrant(t *testing.T) {
	granted := map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read", "statuses": "write"} // no issues, no repository_hooks
	r := newGrantMintRecorder(t, granted)
	a := r.appClient(t, granted)
	ctx := context.Background()
	if _, err := a.ListRepos(ctx, forge.RepoQuery{}); err != nil {
		t.Fatalf("a connection granted a strict subset of the baseline must still mint the intersection: %v", err)
	}
	mints := r.snapshot()
	if len(mints) != 1 {
		t.Fatalf("mints = %d, want exactly 1 (no refused first attempt)", len(mints))
	}
	if !reflect.DeepEqual(mints[0], granted) {
		t.Errorf("minted permissions = %v, want the grant ∩ baseline + statuses = %v", mints[0], granted)
	}
	// What the narrowing dropped is denied up front — no round trip needed to
	// learn it — so a lane that needs it can switch credential before acting.
	for _, p := range []string{"issues", "repository_hooks"} {
		if err := a.PreflightFor(ctx, p); !errors.Is(err, forge.ErrPermissionsNotGranted) {
			t.Errorf("PreflightFor(%s) = %v, want ErrPermissionsNotGranted: the grant lacks it", p, err)
		}
	}
	if err := a.PreflightFor(ctx, PermissionStatuses); err != nil {
		t.Errorf("PreflightFor(statuses) = %v, want nil: the grant carries it", err)
	}
}

// A grant that lacks statuses skips the write request instead of paying a
// refused mint plus a retry, and PreflightFor reports it withheld.
func TestAppClientManagementMintSkipsStatusesTheGrantLacks(t *testing.T) {
	granted := RuntimeInstallationPermissions() // the pre-merge-gate installation shape
	r := newGrantMintRecorder(t, granted)
	a := r.appClient(t, granted)
	ctx := context.Background()
	if _, err := a.ListRepos(ctx, forge.RepoQuery{}); err != nil {
		t.Fatal(err)
	}
	mints := r.snapshot()
	if len(mints) != 1 {
		t.Fatalf("mints = %d, want 1 (no refused attempt for a grant known to lack statuses)", len(mints))
	}
	if _, ok := mints[0][PermissionStatuses]; ok {
		t.Errorf("minted permissions = %v, must not ask for statuses the grant lacks", mints[0])
	}
	if err := a.PreflightFor(ctx, PermissionStatuses); !errors.Is(err, forge.ErrPermissionsNotGranted) {
		t.Errorf("PreflightFor(statuses) = %v, want ErrPermissionsNotGranted", err)
	}
}

// An unknown grant (a connection stored before grants were recorded) keeps
// the historical request — baseline plus the optional statuses, retried
// without statuses when refused — because absence of data is not evidence
// of a narrower installation.
func TestAppClientManagementMintUnknownGrantKeepsTheBaseline(t *testing.T) {
	r := newGrantMintRecorder(t, nil)
	a := r.appClient(t, nil)
	if _, err := a.ListRepos(context.Background(), forge.RepoQuery{}); err != nil {
		t.Fatal(err)
	}
	mints := r.snapshot()
	want := RuntimeInstallationPermissions()
	want[PermissionStatuses] = "write"
	if len(mints) != 1 || !reflect.DeepEqual(mints[0], want) {
		t.Errorf("mints = %v, want one mint of the baseline + statuses:write", mints)
	}
}

// ManagementPermissionsFor is the narrowing rule itself.
func TestManagementPermissionsFor(t *testing.T) {
	base := RuntimeInstallationPermissions()
	if got := ManagementPermissionsFor(nil); !reflect.DeepEqual(got, base) {
		t.Errorf("unknown grant = %v, want the baseline %v", got, base)
	}
	granted := map[string]string{"contents": "write", "metadata": "read", "workflows": "write", "packages": "write"}
	got := ManagementPermissionsFor(granted)
	if !reflect.DeepEqual(got, map[string]string{"contents": "write", "metadata": "read"}) {
		t.Errorf("got %v, want the baseline ∩ grant", got)
	}
	for _, delivery := range []string{"workflows", "packages"} {
		if _, ok := got[delivery]; ok {
			t.Errorf("the management token must not acquire %s: that grant is the RUN token's (RuntimePermissionsFor)", delivery)
		}
	}
	if got := ManagementPermissionsFor(map[string]string{"something_else": "read"}); !reflect.DeepEqual(got, base) {
		t.Errorf("a grant that covers nothing we recognise keeps the baseline (an empty map would mint the installation's FULL set), got %v", got)
	}
}
