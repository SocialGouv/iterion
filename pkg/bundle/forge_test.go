package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func writeManifestForTest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadManifest_ParsesForgeBlock(t *testing.T) {
	body := `name: review-pr
schema_version: 1
forge:
  events: [pull_request, pull_request_comment]
  token_scopes:
    pull_requests: write
    repository: read
  secret: forge_token
  webhook:
    launch_vars:
      pr_review_mode: summary
      post_to_board: "false"
    min_replier_role: developer
  rationale: |
    Posts a forge review.
`
	m, err := LoadManifest(writeManifestForTest(t, body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Forge == nil {
		t.Fatal("forge block not parsed")
	}
	if got := len(m.Forge.Events); got != 2 {
		t.Fatalf("events: want 2, got %d (%v)", got, m.Forge.Events)
	}
	if m.Forge.TokenScopes["pull_requests"] != "write" {
		t.Errorf("pull_requests scope: %q", m.Forge.TokenScopes["pull_requests"])
	}
	if m.Forge.SecretName() != "forge_token" {
		t.Errorf("secret name: %q", m.Forge.SecretName())
	}
	if m.Forge.Webhook == nil || m.Forge.Webhook.LaunchVars["post_to_board"] != "false" {
		t.Errorf("webhook launch_vars not parsed: %+v", m.Forge.Webhook)
	}
	if m.Forge.Webhook.MinReplierRole != "developer" {
		t.Errorf("min_replier_role: %q", m.Forge.Webhook.MinReplierRole)
	}
}

func TestLoadManifest_NoForgeBlockIsNil(t *testing.T) {
	m, err := LoadManifest(writeManifestForTest(t, "name: plain\nschema_version: 1\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Forge != nil {
		t.Errorf("expected nil Forge, got %+v", m.Forge)
	}
}

func TestLoadManifest_RejectsUnknownForgeEvent(t *testing.T) {
	body := "name: bad\nschema_version: 1\nforge:\n  events: [pull_request, push]\n"
	_, err := LoadManifest(writeManifestForTest(t, body))
	errContains(t, err, `unknown event "push"`)
}

func TestLoadManifest_RejectsUnknownForgeScope(t *testing.T) {
	body := "name: bad\nschema_version: 1\nforge:\n  token_scopes:\n    deploy: write\n"
	_, err := LoadManifest(writeManifestForTest(t, body))
	errContains(t, err, `unknown scope "deploy"`)
}

func TestLoadManifest_RejectsInvalidForgeScopeLevel(t *testing.T) {
	body := "name: bad\nschema_version: 1\nforge:\n  token_scopes:\n    repository: superuser\n"
	_, err := LoadManifest(writeManifestForTest(t, body))
	errContains(t, err, `invalid level "superuser"`)
}

func TestLoadManifest_RejectsUnknownForgeField(t *testing.T) {
	// Strict unmarshal must reject a typo'd key inside the forge block
	// (e.g. token_scope instead of token_scopes) — a silent drop would
	// let a bot under-declare its needs.
	body := "name: bad\nschema_version: 1\nforge:\n  token_scope:\n    repository: read\n"
	_, err := LoadManifest(writeManifestForTest(t, body))
	if err == nil {
		t.Fatal("expected strict-unmarshal error for unknown forge field")
	}
}

func TestForgeRequirements_SecretNameDefault(t *testing.T) {
	if got := (*ForgeRequirements)(nil).SecretName(); got != DefaultForgeSecretName {
		t.Errorf("nil receiver: want %q, got %q", DefaultForgeSecretName, got)
	}
	if got := (&ForgeRequirements{Secret: "  "}).SecretName(); got != DefaultForgeSecretName {
		t.Errorf("blank secret: want %q, got %q", DefaultForgeSecretName, got)
	}
	if got := (&ForgeRequirements{Secret: "gl_pat"}).SecretName(); got != "gl_pat" {
		t.Errorf("explicit secret: want gl_pat, got %q", got)
	}
}

// The actions scope is the workflow-dispatch opt-in (#3130): accepted at
// every level like the other scopes, and named — with every other scope —
// in the refusal an unknown one draws, so the message cannot lag the set
// validation accepts.
func TestLoadManifest_AcceptsTheActionsScopeAndNamesEveryScopeOnARefusal(t *testing.T) {
	m, err := LoadManifest(writeManifestForTest(t, "name: kick\nschema_version: 1\nforge:\n  token_scopes:\n    actions: write\n"))
	if err != nil {
		t.Fatalf("a manifest declaring actions: write = %v, want accepted", err)
	}
	if got := m.Forge.TokenScopes[ForgeScopeActions]; got != "write" {
		t.Fatalf("token_scopes[%s] = %q, want write", ForgeScopeActions, got)
	}
	_, err = LoadManifest(writeManifestForTest(t, "name: bad\nschema_version: 1\nforge:\n  token_scopes:\n    deploy: write\n"))
	for _, scope := range knownForgeScopeNames() {
		errContains(t, err, scope)
	}
}

// A manifest declaring the actions scope never chooses the workflow its
// schedule dispatches: an invocation default_vars naming it — schedule or
// keepalive — is refused at decode, naming the var; the same default is
// accepted where the scope is not declared, and the operator's own row stays
// the only source.
//
// Mutation witnessed: the validateDispatchWorkflowDefault call removed from
// decodeManifest → the schedule default decodes → red.
func TestLoadManifest_TheActionsScopeRefusesAWorkflowDefault(t *testing.T) {
	const scope = "forge:\n  token_scopes:\n    actions: write\n"
	for name, inv := range map[string]string{
		"schedule":  "invocations:\n  - kind: schedule\n    mode: direct\n    schedule: { suggested_cron: \"7 * * * *\", default_vars: { workflow: deploy-prod.yml } }\n",
		"keepalive": "invocations:\n  - kind: keepalive\n    mode: direct\n    keepalive: { interval: 5m, default_vars: { workflow: deploy-prod.yml } }\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadManifest(writeManifestForTest(t, "name: kick\nschema_version: 1\n"+scope+inv))
			errContains(t, err, DispatchWorkflowVar)
			errContains(t, err, "never supplies it")
			if _, err := LoadManifest(writeManifestForTest(t, "name: kick\nschema_version: 1\n"+inv)); err != nil {
				t.Fatalf("the same default without the actions scope = %v, want accepted (it binds nothing)", err)
			}
		})
	}
}
