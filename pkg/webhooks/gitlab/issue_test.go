package gitlab

import (
	"reflect"
	"testing"
)

// glLabeledIssue is the wire shape GitLab sends on an "Issue Hook" when a
// label is added: action "update" with the labels diff in changes.labels.
const glLabeledIssue = `{
  "object_kind": "issue",
  "event_type": "issue",
  "user": {"id": 9, "username": "maintainer-bob"},
  "project": {
    "id": 42,
    "path_with_namespace": "acme/widgets",
    "git_http_url": "https://gitlab.com/acme/widgets.git",
    "default_branch": "main"
  },
  "object_attributes": {
    "iid": 42,
    "title": "Add a CSV export endpoint",
    "description": "Users need their data as CSV.",
    "state": "opened",
    "action": "update",
    "url": "https://gitlab.com/acme/widgets/-/issues/42"
  },
  "labels": [{"title": "implement"}],
  "changes": {
    "labels": {
      "previous": [],
      "current": [{"title": "implement"}]
    }
  }
}`

func TestParseIssue_GitLabLabeled(t *testing.T) {
	p, err := ParseIssue([]byte(glLabeledIssue))
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectID != 42 || p.ProjectPath != "acme/widgets" || p.CloneURL != "https://gitlab.com/acme/widgets.git" || p.DefaultBranch != "main" {
		t.Fatalf("project: %+v", p)
	}
	if p.IssueIID != 42 || p.Action != "update" || p.State != "opened" {
		t.Fatalf("issue: %+v", p)
	}
	if p.Title != "Add a CSV export endpoint" || p.URL != "https://gitlab.com/acme/widgets/-/issues/42" {
		t.Fatalf("fields: %+v", p)
	}
	if !reflect.DeepEqual(p.AddedLabels, []string{"implement"}) {
		t.Fatalf("added labels: %v", p.AddedLabels)
	}
	if p.SubjectID() != "issue:42" || p.AuthorUsername != "maintainer-bob" {
		t.Fatalf("subject/author: %+v", p)
	}
}

func TestParseIssue_NotIssueKindFails(t *testing.T) {
	if _, err := ParseIssue([]byte(`{"object_kind":"note"}`)); err == nil {
		t.Fatal("non-issue object_kind should error")
	}
}

func TestAddedLabels(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"adds one", glLabeledIssue, []string{"implement"}},
		{"no labels diff (non-label update)", `{"object_kind":"issue","object_attributes":{"iid":1,"state":"opened"},"changes":{}}`, nil},
		{"removes only (current⊂previous)",
			`{"object_kind":"issue","object_attributes":{"iid":1,"state":"opened"},"changes":{"labels":{"previous":[{"title":"a"},{"title":"b"}],"current":[{"title":"a"}]}}}`,
			nil},
		{"adds among existing",
			`{"object_kind":"issue","object_attributes":{"iid":1,"state":"opened"},"changes":{"labels":{"previous":[{"title":"a"}],"current":[{"title":"a"},{"title":"implement"}]}}}`,
			[]string{"implement"}},
	}
	for _, c := range cases {
		p, err := ParseIssue([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(p.AddedLabels, c.want) {
			t.Errorf("%s: addedLabels = %v, want %v", c.name, p.AddedLabels, c.want)
		}
	}
}

func TestParseIssue_CurrentLabels(t *testing.T) {
	body := `{"object_kind":"issue","project":{"path_with_namespace":"g/p"},"object_attributes":{"iid":3,"action":"update","state":"opened"},"labels":[{"title":"bug"},{"title":"iterion:hold"}],"changes":{"labels":{"previous":[{"title":"bug"}],"current":[{"title":"bug"},{"title":"iterion:hold"}]}}}`
	p, err := ParseIssue([]byte(body))
	if err != nil {
		t.Fatalf("ParseIssue: %v", err)
	}
	if len(p.Labels) != 2 || p.Labels[0] != "bug" || p.Labels[1] != "iterion:hold" {
		t.Fatalf("current Labels = %v, want [bug iterion:hold]", p.Labels)
	}
	if len(p.AddedLabels) != 1 || p.AddedLabels[0] != "iterion:hold" {
		t.Fatalf("AddedLabels = %v, want [iterion:hold]", p.AddedLabels)
	}
}
