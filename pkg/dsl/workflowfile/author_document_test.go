package workflowfile

import "testing"

// An author document is named by its suffix and is NOT a workflow file: the
// two predicates must disagree on it, or every surface that launches, lists,
// packs or stores a workflow would take a draft for the truth the moment
// someone appended the suffix to Extensions.
func TestAnAuthorDocumentIsNamedAndIsNotAWorkflowFile(t *testing.T) {
	for _, tc := range []struct {
		path           string
		author, workfl bool
	}{
		{"x.bot.yaml", true, false},
		{"bots/foo/main.bot.yaml", true, false},
		{"DRAFT.BOT.YAML", true, false},
		{"draft.Bot.Yaml", true, false},
		{"x.bot", false, true},
		{"x.yaml", false, false},
		{"x.bot.yml", false, false},
		{"x.bot.yaml.bak", false, false},
		{".bot.yaml", true, false},
		{"", false, false},
	} {
		if got := IsAuthorDocument(tc.path); got != tc.author {
			t.Errorf("IsAuthorDocument(%q) = %v, want %v", tc.path, got, tc.author)
		}
		if got := IsWorkflowFile(tc.path); got != tc.workfl {
			t.Errorf("IsWorkflowFile(%q) = %v, want %v", tc.path, got, tc.workfl)
		}
	}
	for _, ext := range Extensions {
		if ext == AuthorExtension {
			t.Fatalf("the author document's suffix is listed among the workflow extensions: every launcher would accept a draft")
		}
	}
}
