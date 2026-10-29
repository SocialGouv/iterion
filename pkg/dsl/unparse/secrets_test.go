package unparse

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

const secretsRoundtripSrc = `secrets:
  github_token: "${GITHUB_TOKEN}"
  deploy_key:
    value: "${DEPLOY_KEY}"
    hosts: ["api.github.com", "github.com"]
    sinks: [prompt, egress]
    description: "deploy key"
  kubeconfig:
    as: file
    value: "${KUBECONFIG}"
    mount_path: "/run/iterion/secrets/kubeconfig"
    env: "KUBECONFIG"
  db_password:
    value: "${DB_PASSWORD}"
    sinks: [command]

agent x:
  model: "anthropic/c"
  system: p

prompt p:
  Use {{secrets.github_token}}.

workflow w:
  entry: x
  x -> done
`

func TestSecretsRoundtrip(t *testing.T) {
	pr1 := parsertest.Parse("t.bot", secretsRoundtripSrc)
	if len(pr1.Diagnostics) > 0 {
		t.Fatalf("first parse diagnostics: %+v", pr1.Diagnostics)
	}
	out1 := Unparse(pr1.File)

	pr2 := parsertest.Parse("t.bot", out1)
	if len(pr2.Diagnostics) > 0 {
		t.Fatalf("re-parse produced diagnostics:\n%s\n%+v", out1, pr2.Diagnostics)
	}
	out2 := Unparse(pr2.File)
	if out1 != out2 {
		t.Fatalf("round-trip drift:\n--- pass 1 ---\n%s\n--- pass 2 ---\n%s", out1, out2)
	}

	for _, want := range []string{
		"secrets:",
		`github_token: "${GITHUB_TOKEN}"`,
		"deploy_key:",
		`value: "${DEPLOY_KEY}"`,
		`hosts: ["api.github.com", "github.com"]`,
		`sinks: [prompt, egress]`,
		"db_password:",
		`sinks: [command]`,
		"kubeconfig:",
		"as: file",
		`mount_path: "/run/iterion/secrets/kubeconfig"`,
		`env: "KUBECONFIG"`,
	} {
		if !strings.Contains(out1, want) {
			t.Errorf("unparsed output missing %q:\n%s", want, out1)
		}
	}
}

func TestSecretsASTJSONRoundtrip(t *testing.T) {
	pr := parsertest.Parse("t.bot", secretsRoundtripSrc)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parse diagnostics: %+v", pr.Diagnostics)
	}
	blob, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	if !strings.Contains(string(blob), "github_token") {
		t.Fatalf("AST JSON dropped the secrets block:\n%s", blob)
	}
	back, err := ast.UnmarshalFile(blob)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}
	if back.Secrets == nil || len(back.Secrets.Fields) != 4 {
		t.Fatalf("secrets lost through AST JSON round-trip: %+v", back.Secrets)
	}
	if db := back.Secrets.Fields[3]; db.Name != "db_password" || strings.Join(db.Sinks, ",") != "command" {
		t.Errorf("a secret with only value: and sinks: lost its grant through AST JSON: %+v", db)
	}
	dk := back.Secrets.Fields[1]
	if dk.Name != "deploy_key" || len(dk.Hosts) != 2 || dk.Value != "${DEPLOY_KEY}" {
		t.Errorf("deploy_key not preserved: %+v", dk)
	}
	if strings.Join(dk.Sinks, ",") != "prompt,egress" {
		t.Errorf("deploy_key sinks not preserved through AST JSON: %+v", dk.Sinks)
	}
	kc := back.Secrets.Fields[2]
	if kc.Name != "kubeconfig" || kc.As != "file" || kc.MountPath != "/run/iterion/secrets/kubeconfig" || kc.Env != "KUBECONFIG" {
		t.Errorf("kubeconfig file metadata not preserved: %+v", kc)
	}
}
