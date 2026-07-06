package ir

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestSecretsBlockCompiles(t *testing.T) {
	src := `
secrets:
  github_token: "${GITHUB_TOKEN_TEST}"
  deploy_key:
    value: "${DEPLOY_KEY_TEST}"
    hosts: ["api.github.com", "github.com"]

agent x:
  model: "anthropic/c"
  system: p

prompt p:
  Use {{secrets.github_token}} to authenticate.

workflow w:
  entry: x
  x -> done
`
	pr := parser.Parse("t.bot", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("compile errors: %+v", cr.Diagnostics)
	}
	secrets := cr.Workflow.Secrets
	if len(secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d: %+v", len(secrets), secrets)
	}
	if got := secrets["github_token"].Value; got != "${GITHUB_TOKEN_TEST}" {
		t.Errorf("github_token value = %q", got)
	}
	dk := secrets["deploy_key"]
	if dk == nil || dk.Value != "${DEPLOY_KEY_TEST}" {
		t.Fatalf("deploy_key not compiled: %+v", dk)
	}
	if len(dk.Hosts) != 2 || dk.Hosts[0] != "api.github.com" {
		t.Errorf("deploy_key hosts = %+v", dk.Hosts)
	}
}

func TestFileSecretCompilesAndPathRefValidates(t *testing.T) {
	src := `
secrets:
  kubeconfig:
    as: file
    value: "${KUBECONFIG_TEST}"
    mount_path: "/run/iterion/secrets/kubeconfig"
    env: "KUBECONFIG"
    hosts: ["api.cluster.example"]

agent x:
  model: "anthropic/c"
  system: p

prompt p:
  Use {{secrets.kubeconfig.path}} for kubectl.

workflow w:
  entry: x
  x -> done
`
	pr := parser.Parse("t.bot", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("compile errors: %+v", cr.Diagnostics)
	}
	got := cr.Workflow.Secrets["kubeconfig"]
	if got == nil {
		t.Fatal("kubeconfig secret not compiled")
	}
	if !got.IsFile() || got.MountPath != "/run/iterion/secrets/kubeconfig" || got.Env != "KUBECONFIG" {
		t.Fatalf("file secret metadata not preserved: %+v", got)
	}
}

func TestFileSecretValidationDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		src  string
		code DiagCode
	}{
		{
			name: "path on value secret",
			src: `
secrets:
  token: "${TOKEN_TEST}"
agent a:
  model: "anthropic/c"
  system: p
prompt p:
  {{secrets.token.path}}
workflow w:
  entry: a
  a -> done
`,
			code: DiagSecretSubfield,
		},
		{
			name: "mount path requires file mode",
			src: `
secrets:
  token:
    value: "${TOKEN_TEST}"
    mount_path: "/run/iterion/secrets/token"
agent a:
  model: "anthropic/c"
  system: p
prompt p:
  hi
workflow w:
  entry: a
  a -> done
`,
			code: DiagInvalidSecretFile,
		},
		{
			name: "mount path must be clean file path",
			src: `
secrets:
  token:
    as: file
    value: "${TOKEN_TEST}"
    mount_path: "/run/iterion/secrets/../token"
agent a:
  model: "anthropic/c"
  system: p
prompt p:
  hi
workflow w:
  entry: a
  a -> done
`,
			code: DiagInvalidSecretFile,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr := parser.Parse("t.bot", tc.src)
			if len(pr.Diagnostics) > 0 {
				t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
			}
			cr := Compile(pr.File)
			for _, d := range cr.Diagnostics {
				if d.Code == tc.code {
					return
				}
			}
			t.Fatalf("expected %s, got %+v", tc.code, cr.Diagnostics)
		})
	}
}

func TestValidSecretHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"api.github.com", true},
		{"github.com", true},
		{"localhost", true},
		{"127.0.0.1", true},
		{"2001:db8::1", true},
		{"https://api.github.com", false},
		{"api.github.com/path", false},
		{"api.github.com:443", false},
		{"*.github.com", false},
		{"github.com.", false},
		{"bad_label.example", false},
		{"-bad.example", false},
		{"[2001:db8::1]:443", false},
	}
	for _, tc := range cases {
		if got := validSecretHost(tc.host); got != tc.want {
			t.Errorf("validSecretHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestSecretsUnknownRefDiagnostic(t *testing.T) {
	src := `
secrets:
  known: "${X_TEST}"

agent a:
  model: "anthropic/c"
  system: p

prompt p:
  {{secrets.unknown}} should fail

workflow w:
  entry: a
  a -> done
`
	pr := parser.Parse("t.bot", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parser diagnostics: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if !hasDiag(cr.Diagnostics, DiagUnknownSecret) {
		t.Fatalf("expected DiagUnknownSecret (C093), got %+v", cr.Diagnostics)
	}
}

func TestSecretsDuplicateDiagnostic(t *testing.T) {
	src := `
secrets:
  dup: "${A_TEST}"
  dup: "${B_TEST}"

agent a:
  model: "anthropic/c"
  system: p

prompt p:
  hi

workflow w:
  entry: a
  a -> done
`
	pr := parser.Parse("t.bot", src)
	cr := Compile(pr.File)
	if !hasDiag(cr.Diagnostics, DiagDuplicateSecret) {
		t.Fatalf("expected DiagDuplicateSecret (C090), got %+v", cr.Diagnostics)
	}
}
