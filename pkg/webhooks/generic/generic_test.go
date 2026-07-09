package generic

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestParseRequest_HappyPath(t *testing.T) {
	body := []byte(`{
	  "bot": "review-pr",
	  "vars": {"pr_url": "https://example.com/pr/1", "scope_notes": "x"},
	  "idempotency_key": "evt-1",
	  "repo_url": "https://example.com/foo.git",
	  "repo_ref": "main",
	  "project_path": "foo/bar"
	}`)
	r, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if r.Bot != "review-pr" || r.Vars["pr_url"] != "https://example.com/pr/1" || r.IdempotencyKey != "evt-1" {
		t.Fatalf("parsed: %+v", r)
	}
	if r.RepoURL == "" || r.RepoRef != "main" || r.ProjectPath != "foo/bar" {
		t.Fatalf("repo/project: %+v", r)
	}
}

func TestParseRequest_RejectsBadVarKey(t *testing.T) {
	body := []byte(`{"vars": {"with space": "x"}}`)
	_, err := ParseRequest(body)
	if !errors.Is(err, ErrBadVarKey) {
		t.Fatalf("want ErrBadVarKey, got %v", err)
	}
}

func TestParseRequest_RejectsOversizedVar(t *testing.T) {
	val := strings.Repeat("x", MaxVarValueSize+1)
	body := []byte(`{"vars": {"k": "` + val + `"}}`)
	_, err := ParseRequest(body)
	if !errors.Is(err, ErrVarValueTooLarge) {
		t.Fatalf("want ErrVarValueTooLarge, got %v", err)
	}
}

func TestParseRequest_RejectsTooManyVars(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"vars": {`)
	for i := 0; i < MaxVars+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmtKey := "k" + strconv.Itoa(i)
		b.WriteString(`"` + fmtKey + `": "v"`)
	}
	b.WriteString(`}}`)
	_, err := ParseRequest([]byte(b.String()))
	if !errors.Is(err, ErrTooManyVars) {
		t.Fatalf("want ErrTooManyVars, got %v", err)
	}
}

func TestParseRequest_RejectsMalformedJSON(t *testing.T) {
	if _, err := ParseRequest([]byte(`{bad`)); err == nil {
		t.Fatal("malformed json should error")
	}
}
