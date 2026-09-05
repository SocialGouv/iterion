package github

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// A token that can rewrite an org's roadmap is a broader privilege than one
// that can push a branch. These tests pin that the project-board grant is a
// SEPARATE opt-in profile and never leaks into the runtime baseline — the same
// shape as the security-read and repo-admin precedents.

func TestProjectsInstallationPermissionsIsItsOwnProfile(t *testing.T) {
	perms := ProjectsInstallationPermissions()
	if perms["organization_projects"] != "write" {
		t.Errorf("projects profile must carry organization_projects:write, got %v", perms)
	}
	if perms["metadata"] != "read" {
		t.Errorf("projects profile must carry the mandatory metadata baseline, got %v", perms)
	}
	if _, ok := RuntimeInstallationPermissions()["organization_projects"]; ok {
		t.Error("organization_projects must NOT be in the runtime baseline: the cached runtime token stays minimal")
	}
}

func TestBuildAppManifestProjectBoardIsOptIn(t *testing.T) {
	base := BuildAppManifest("it", "https://x", "https://x/cb")
	if _, ok := base.DefaultPermissions["organization_projects"]; ok {
		t.Error("default manifest must not request organization_projects")
	}

	opted := BuildAppManifest("it", "https://x", "https://x/cb", AppManifestOptions{AllowProjectBoard: true})
	if opted.DefaultPermissions["organization_projects"] != "write" {
		t.Errorf("AllowProjectBoard must request organization_projects:write, got %v", opted.DefaultPermissions)
	}
	// The baseline must survive alongside it.
	if opted.DefaultPermissions["contents"] != "write" {
		t.Error("the runtime baseline must remain when the project grant is added")
	}

	// A watch-only App replaces the whole set: no write grant may sneak in.
	watch := BuildAppManifest("it", "https://x", "https://x/cb", AppManifestOptions{SecurityReadOnly: true, AllowProjectBoard: true})
	if _, ok := watch.DefaultPermissions["organization_projects"]; ok {
		t.Errorf("a security-read-only App must carry no project grant, got %v", watch.DefaultPermissions)
	}
}

func TestMissingProjectPermissions(t *testing.T) {
	if got := MissingProjectPermissions(nil); got != nil {
		t.Errorf("an unknown grant set is not evidence of a gap, got %v", got)
	}
	granted := map[string]string{"contents": "write", "metadata": "read"}
	got := MissingProjectPermissions(granted)
	if len(got) != 1 || got[0] != "organization_projects" {
		t.Errorf("MissingProjectPermissions = %v, want [organization_projects]", got)
	}
	granted["organization_projects"] = "write"
	if got := MissingProjectPermissions(granted); got != nil {
		t.Errorf("nothing is missing once the grant is present, got %v", got)
	}
}

// TestAppClientIsABoardClient pins the parity half: a GitHub-App connection
// must reach the board too, or the capability silently works on PATs only.
func TestAppClientIsABoardClient(t *testing.T) {
	var admin forge.Admin = &AppClient{}
	if _, ok := forge.AsBoardClient(admin); !ok {
		t.Fatal("github AppClient must implement forge.BoardClient")
	}
}
