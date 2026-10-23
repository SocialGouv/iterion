package ir

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"testing"
)

func TestOptionalFileSecretCompiles(t *testing.T) {
	src := `
secrets:
  gitlab_token:
    as: file
    optional: true
    hosts: ["gitlab.com"]

agent x:
  model: "anthropic/c"
  system: p

prompt p:
  hi

workflow w:
  entry: x
  x -> done
`
	pr := parsertest.Parse("t.bot", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parse: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("compile: %+v", cr.Diagnostics)
	}
	s := cr.Workflow.Secrets["gitlab_token"]
	if s == nil || !s.IsFile() || !s.Optional {
		t.Fatalf("optional file secret not compiled: %+v", s)
	}
	if len(s.Hosts) != 1 || s.Hosts[0] != "gitlab.com" {
		t.Fatalf("hosts: %+v", s.Hosts)
	}
}
