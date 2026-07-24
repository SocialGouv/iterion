package prforge

import "testing"

// githubLabeledIssue is the wire shape GitHub sends on `issues` with the
// "labeled" action: the issue plus the single .label just applied.
const githubLabeledIssue = `{
  "action": "labeled",
  "issue": {
    "number": 42,
    "title": "Add a CSV export endpoint",
    "body": "Users need to download their data as CSV.",
    "html_url": "https://github.com/acme/widgets/issues/42",
    "state": "open"
  },
  "label": {"name": "implement"},
  "repository": {
    "id": 7,
    "full_name": "acme/widgets",
    "clone_url": "https://github.com/acme/widgets.git",
    "html_url": "https://github.com/acme/widgets"
  },
  "sender": {"login": "maintainer-bob"}
}`

func TestParseIssues_GitHubLabeled(t *testing.T) {
	p, err := ParseIssues([]byte(githubLabeledIssue))
	if err != nil {
		t.Fatal(err)
	}
	if p.RepoID != 7 || p.ProjectPath != "acme/widgets" || p.CloneURL != "https://github.com/acme/widgets.git" {
		t.Fatalf("repo: %+v", p)
	}
	if p.IssueNumber != 42 || p.Action != "labeled" || p.LabelName != "implement" {
		t.Fatalf("issue/label: %+v", p)
	}
	if p.IssueTitle != "Add a CSV export endpoint" || p.IssueURL != "https://github.com/acme/widgets/issues/42" || p.IssueState != "open" {
		t.Fatalf("fields: %+v", p)
	}
	if p.SubjectID() != "issue:42" || p.SenderLogin != "maintainer-bob" {
		t.Fatalf("subject/sender: %+v", p)
	}
	if !p.IsLabeled() {
		t.Fatal("labeled issue with a label should be actionable")
	}
}

func TestParseIssues_MalformedFails(t *testing.T) {
	if _, err := ParseIssues([]byte(`{bad`)); err == nil {
		t.Fatal("malformed json should error")
	}
}

func TestIssuesIsLabeled(t *testing.T) {
	cases := []struct {
		action string
		label  string
		want   bool
	}{
		{"labeled", "implement", true},
		{"labeled", "", false}, // labeled but no label name → not actionable
		{"unlabeled", "implement", false},
		{"opened", "", false},
		{"closed", "", false},
		{"assigned", "implement", false},
	}
	for _, c := range cases {
		p := ParsedIssue{Action: c.action, LabelName: c.label}
		if got := p.IsLabeled(); got != c.want {
			t.Errorf("action=%q label=%q => %v want %v", c.action, c.label, got, c.want)
		}
	}
}

func TestParseIssues_Labels(t *testing.T) {
	body := `{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":9,"labels":[{"name":"bug"},{"name":"blocked"}]}}`
	p, err := ParseIssues([]byte(body))
	if err != nil {
		t.Fatalf("ParseIssues: %v", err)
	}
	if len(p.IssueLabels) != 2 || p.IssueLabels[0] != "bug" || p.IssueLabels[1] != "blocked" {
		t.Fatalf("issue labels = %v, want [bug blocked]", p.IssueLabels)
	}
}
