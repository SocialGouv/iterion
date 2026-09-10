package cli

import (
	"strings"
	"testing"
)

// The `--project` half of `iterion issue import` must fail FAST and say why:
// a malformed ref or a provider with no project board is an operator typo, and
// discovering it after a full issue sync (or worse, as a silent no-op) wastes
// the run and hides the mistake.

func newProjectImportOpts(t *testing.T) IssueImportOptions {
	t.Helper()
	t.Setenv("ITERION_TEST_FORGE_TOKEN", "tok")
	return IssueImportOptions{
		IssueCommonOptions: IssueCommonOptions{StoreDir: t.TempDir()},
		Forge:              "github",
		Repo:               "SocialGouv/iterion",
		TokenEnv:           "ITERION_TEST_FORGE_TOKEN",
	}
}

func TestIssueImportRejectsMalformedProjectRef(t *testing.T) {
	opts := newProjectImportOpts(t)
	opts.Project = "SocialGouv"

	err := RunIssueImport(NewPrinter(OutputJSON), opts)
	if err == nil {
		t.Fatal("want an error for a project ref without a number")
	}
	if !strings.Contains(err.Error(), "owner") || !strings.Contains(err.Error(), "number") {
		t.Errorf("the error must name the expected form, got %q", err)
	}
}

func TestIssueImportRejectsUnknownProjectOwnerKind(t *testing.T) {
	opts := newProjectImportOpts(t)
	opts.Project = "SocialGouv/203"
	opts.ProjectOwnerKind = "group"

	err := RunIssueImport(NewPrinter(OutputJSON), opts)
	if err == nil {
		t.Fatal("want an error for an unknown owner kind")
	}
	if !strings.Contains(err.Error(), "org") || !strings.Contains(err.Error(), "user") {
		t.Errorf("the error must name the accepted kinds, got %q", err)
	}
}

func TestIssueImportRejectsProjectOnAProviderWithoutOne(t *testing.T) {
	opts := newProjectImportOpts(t)
	opts.Forge = "forgejo"
	opts.BaseURL = "https://forge.example.com"
	opts.Project = "someone/1"

	err := RunIssueImport(NewPrinter(OutputJSON), opts)
	if err == nil {
		t.Fatal("want an error: forgejo exposes no project board")
	}
	if !strings.Contains(err.Error(), "project board") {
		t.Errorf("the error must say the provider has no project board, got %q", err)
	}
}
