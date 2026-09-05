package dispatcher

import (
	"strings"
	"testing"
)

// The dispatcher config is the only way an operator reaches board mode from
// `iterion dispatch`. A `project:` block the factory ignored would be dead
// config: written, read back, and silently doing nothing.

func TestGitHubTrackerConfigWiresBoardMode(t *testing.T) {
	tr, err := buildGitHubTrackerFromConfig(&GitHubTrackerConfig{
		Repo:  "SocialGouv/iterion",
		Token: "tok",
		Project: &GitHubProjectConfig{
			Owner: "SocialGouv", Number: 203,
			CandidateStatuses: []string{"Planned"},
		},
	})
	if err != nil {
		t.Fatalf("buildGitHubTrackerFromConfig: %v", err)
	}
	if tr == nil {
		t.Fatal("nil tracker")
	}
	if got := tr.Name(); got != "github" {
		t.Errorf("Name() = %q", got)
	}
}

func TestGitHubTrackerConfigProjectNeedsAToken(t *testing.T) {
	_, err := buildGitHubTrackerFromConfig(&GitHubTrackerConfig{
		Repo:    "SocialGouv/iterion",
		Project: &GitHubProjectConfig{Owner: "SocialGouv", Number: 203},
	})
	if err == nil {
		t.Fatal("want an error: the board client is an API client, it cannot borrow the gh login")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("the error must say a token is required, got %q", err)
	}
}

func TestGitHubTrackerConfigRejectsAnIncompleteProject(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    GitHubProjectConfig
	}{
		{"no owner", GitHubProjectConfig{Number: 1}},
		{"no number", GitHubProjectConfig{Owner: "o"}},
		{"bad owner kind", GitHubProjectConfig{Owner: "o", Number: 1, OwnerKind: "group"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			_, err := buildGitHubTrackerFromConfig(&GitHubTrackerConfig{
				Repo: "o/r", Token: "tok", Project: &p,
			})
			if err == nil {
				t.Fatal("want a config error")
			}
		})
	}
}
