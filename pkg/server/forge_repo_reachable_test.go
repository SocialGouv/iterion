package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The regression this encodes: iterion created SocialGouv/appy-quotes-live via
// the App's administration:write token, but GitHub does NOT add an App-created
// repo to a "selected repositories" installation. The run launched fine and
// only failed hours later, at `git push`, with a bare 403.
func TestRepoOutsideInstallationErr(t *testing.T) {
	installed := []forge.RepoSummary{
		{FullName: "SocialGouv/iterion"},
		{FullName: "SocialGouv/iterion-veille"},
	}

	t.Run("repo absent from the installation is a definitive negative", func(t *testing.T) {
		err := repoOutsideInstallationErr(installed, "SocialGouv/appy-quotes-live")
		if err == nil {
			t.Fatal("want an error for a repo outside the installation")
		}
		// The message must name the repo AND the remedy — this error exists to
		// stop an operator debugging the agent instead of the grant.
		for _, want := range []string{"appy-quotes-live", "Repository access", "All repositories"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("repo in the installation passes", func(t *testing.T) {
		if err := repoOutsideInstallationErr(installed, "SocialGouv/iterion"); err != nil {
			t.Errorf("want nil for an installed repo, got %v", err)
		}
	})

	// Best-effort contract: only a definitive negative blocks. An installation
	// we could not enumerate must never block an otherwise valid launch.
	t.Run("no usable answer never blocks", func(t *testing.T) {
		if err := repoOutsideInstallationErr(nil, "SocialGouv/appy-quotes-live"); err != nil {
			t.Errorf("empty repo list must not block, got %v", err)
		}
		if err := repoOutsideInstallationErr(installed, ""); err != nil {
			t.Errorf("empty repo name must not block, got %v", err)
		}
	})

	// The installation lists full names; a launch carries a project path. Both
	// normalise through shortRepoName, including a .git suffix.
	t.Run("matches on the short name across shapes", func(t *testing.T) {
		if err := repoOutsideInstallationErr(installed, "SocialGouv/iterion.git"); err != nil {
			t.Errorf("a .git suffix must still match, got %v", err)
		}
	})
}
