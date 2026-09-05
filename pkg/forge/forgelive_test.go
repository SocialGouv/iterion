//go:build forgelive

// Live validation of the forge admin clients against REAL forges — the exact
// provisioning lifecycle the orchestrator drives (WhoAmI → ListRepos →
// CreateHook → GetHook → UpdateHook → DeleteHook). Build-tagged + env-gated so
// it never runs in normal CI (mirrors `task test:live`).
//
//	# GitLab (PAT)
//	export GITLAB_FABRIQUE_TOKEN=...   # sourced from the secrets .env
//	devbox run -- go test -tags forgelive -run TestLiveGitLab -v ./pkg/forge/
//
//	# GitHub (gh token)
//	FORGE_GITHUB_TOKEN="$(gh auth token)" FORGE_GITHUB_REPO=owner/repo \
//	  devbox run -- go test -tags forgelive -run TestLiveGitHub -v ./pkg/forge/
package forge_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/brand"
	"github.com/SocialGouv/iterion/pkg/forge"
	fgithub "github.com/SocialGouv/iterion/pkg/forge/github"
	fgitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func lastPath(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// hookLifecycle exercises the full provisioning CRUD against a real forge and
// always cleans up the hook it creates. events are provider-native.
func hookLifecycle(t *testing.T, ctx context.Context, admin forge.Admin, repo string, events []string) {
	t.Helper()
	deliveryURL := "https://iterion-forge-livetest.invalid/api/webhooks/" +
		string(admin.Provider()) + "/livetest-" + time.Now().UTC().Format("20060102T150405")

	// Pre-state: no iterion hook at this URL yet.
	if h, err := admin.GetHook(ctx, repo, deliveryURL); err != nil {
		t.Fatalf("GetHook(pre): %v", err)
	} else if h != nil {
		t.Fatalf("GetHook(pre): unexpected existing hook %s", h.ID)
	}

	created, err := admin.CreateHook(ctx, repo, forge.HookSpec{
		URL: deliveryURL, Secret: "iwh_livetest_secret", Events: events, Active: true,
	})
	if err != nil {
		t.Fatalf("CreateHook: %v", err)
	}
	t.Logf("CreateHook OK: id=%s events=%v", created.ID, created.Events)
	// Always clean up, even if a later step fails.
	defer func() {
		if err := admin.DeleteHook(ctx, repo, created.ID); err != nil {
			t.Errorf("DeleteHook(cleanup): %v — MANUAL CLEANUP NEEDED for hook %s on %s", err, created.ID, repo)
			return
		}
		t.Logf("DeleteHook OK: id=%s", created.ID)
		if h, err := admin.GetHook(ctx, repo, deliveryURL); err == nil && h != nil {
			t.Errorf("hook %s still present after delete", h.ID)
		}
	}()

	// GetHook finds it by URL (the orchestrator's idempotency probe).
	found, err := admin.GetHook(ctx, repo, deliveryURL)
	if err != nil {
		t.Fatalf("GetHook(post-create): %v", err)
	}
	if found == nil || found.ID != created.ID {
		t.Fatalf("GetHook(post-create) = %+v, want id %s", found, created.ID)
	}
	t.Logf("GetHook OK: matched id=%s by URL", found.ID)

	// UpdateHook (event-widen path) — narrow to the first event then back.
	updated, err := admin.UpdateHook(ctx, repo, created.ID, forge.HookSpec{
		URL: deliveryURL, Secret: "iwh_livetest_secret", Events: events[:1], Active: true,
	})
	if err != nil {
		t.Fatalf("UpdateHook: %v", err)
	}
	t.Logf("UpdateHook OK: id=%s events=%v", updated.ID, updated.Events)
}

func TestLiveGitLab(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("GITLAB_FABRIQUE_TOKEN"))
	if token == "" {
		t.Skip("GITLAB_FABRIQUE_TOKEN not set")
	}
	base := envOr("FORGE_GITLAB_BASE", "https://gitlab.fabrique.social.gouv.fr")
	repo := envOr("FORGE_GITLAB_REPO", "devthejo/revi-playground")
	ctx := context.Background()
	c := fgitlab.New(nil, base, token)

	id, err := c.WhoAmI(ctx)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	t.Logf("WhoAmI OK: @%s (id %s) on %s", id.Login, id.ID, base)

	repos, err := c.ListRepos(ctx, forge.RepoQuery{Search: lastPath(repo)})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	t.Logf("ListRepos OK: %d admin-capable repos matching %q", len(repos), lastPath(repo))
	var found bool
	for _, r := range repos {
		if r.FullName == repo {
			found = true
			t.Logf("  target repo: %s (private=%v default_branch=%s can_admin=%v)", r.FullName, r.Private, r.DefaultBranch, r.CanAdmin)
		}
	}
	if !found {
		t.Logf("WARN: %q not in the admin-capable list (proceeding with hook lifecycle anyway)", repo)
	}

	hookLifecycle(t, ctx, c, repo, []string{"merge_request", "note"})
}

func TestLiveGitHub(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("FORGE_GITHUB_TOKEN"))
	if token == "" {
		t.Skip("FORGE_GITHUB_TOKEN not set (e.g. FORGE_GITHUB_TOKEN=$(gh auth token))")
	}
	base := envOr("FORGE_GITHUB_BASE", "https://github.com")
	ctx := context.Background()
	c := fgithub.New(nil, base, token)

	id, err := c.WhoAmI(ctx)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	t.Logf("WhoAmI OK: @%s (id %s) on %s", id.Login, id.ID, base)

	repos, err := c.ListRepos(ctx, forge.RepoQuery{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	t.Logf("ListRepos OK: %d admin-capable repos (page 1)", len(repos))

	repo := strings.TrimSpace(os.Getenv("FORGE_GITHUB_REPO"))
	if repo == "" {
		t.Log("FORGE_GITHUB_REPO not set — read-only validation only (set it to run the hook create/delete lifecycle)")
		return
	}
	hookLifecycle(t, ctx, c, repo, []string{"pull_request", "issue_comment"})
}

// TestLiveGitLabAvatar proves PUT /user/avatar on a REAL GitLab for the account
// a token authenticates as — the case that matters is a project/group access
// token, whose bot user is documented without restriction but had never been
// observed taking an avatar. Gated on a token FILE (never the token itself on
// a command line):
//
//	FORGE_GITLAB_AVATAR_TOKEN_FILE=/tmp/probe-token FORGE_GITLAB_BASE=https://pic.sg.social.gouv.fr \
//	  devbox run -- go test -tags forgelive -run TestLiveGitLabAvatar -v ./pkg/forge/
//
// It changes that account's avatar for real: point it at a throwaway token.
func TestLiveGitLabAvatar(t *testing.T) {
	tokenFile := strings.TrimSpace(os.Getenv("FORGE_GITLAB_AVATAR_TOKEN_FILE"))
	if tokenFile == "" {
		t.Skip("FORGE_GITLAB_AVATAR_TOKEN_FILE not set")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	base := envOr("FORGE_GITLAB_BASE", "https://gitlab.fabrique.social.gouv.fr")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	admin := fgitlab.New(nil, base, strings.TrimSpace(string(raw)))
	ident, err := admin.WhoAmI(ctx)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	t.Logf("token authenticates as @%s (id %s, kind %s)", ident.Login, ident.ID, ident.Kind)

	png := brand.BotAvatar(brand.VariantPlain)
	url, err := admin.SetAvatar(ctx, png)
	if err != nil {
		t.Fatalf("SetAvatar as @%s (%s): %v", ident.Login, ident.Kind, err)
	}
	if url == "" {
		t.Fatal("GitLab reported no avatar_url")
	}
	t.Logf("avatar set: %s", url)
	// An uploaded (not gravatar/identicon) avatar lives under the instance's
	// own uploads path.
	if !strings.Contains(url, "/uploads/") {
		t.Errorf("avatar url %q does not look like an uploaded avatar", url)
	}
}
