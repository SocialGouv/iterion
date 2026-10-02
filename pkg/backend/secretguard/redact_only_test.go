package secretguard

import (
	"os"
	"strings"
	"testing"
)

// A RedactOnly value is scrubbed like any other and handed back nowhere: no
// materialiser resolves its placeholder, toward any host.
func TestRedactOnlySecretIsScrubbedAndNeverMaterialised(t *testing.T) {
	const value = "redact-only-value-0123456789abcdef"
	g := New([]Secret{{Name: "ambient", Value: value, RedactOnly: true}}, DefaultConfig())
	ph := PlaceholderForName("ambient")
	if !g.HasKnownSecrets() || g.Materializes() {
		t.Fatalf("HasKnownSecrets=%v Materializes=%v, want true/false", g.HasKnownSecrets(), g.Materializes())
	}
	if red := g.Redact("key=" + value); strings.Contains(red, value) || !strings.Contains(red, ph) {
		t.Fatalf("Redact = %q, want the value replaced by %s", red, ph)
	}
	for name, got := range map[string]string{
		"Materialize":        g.Materialize("x " + ph),
		"MaterializeShell":   g.MaterializeShell("x '" + ph + "'"),
		"MaterializeForHost": g.MaterializeForHost("x "+ph, "any.example"),
		"ResolveSecretRef":   g.ResolveSecretRef("ambient"),
	} {
		if strings.Contains(got, value) {
			t.Errorf("%s resolved a RedactOnly placeholder", name)
		}
	}
	if got, env := g.MaterializeShellEnv("x '" + ph + "'"); strings.Contains(got, value) || len(env) != 0 {
		t.Error("MaterializeShellEnv resolved a RedactOnly placeholder")
	}
}

// A materialisable secret keeps resolving, toward the hosts it is scoped to.
func TestMaterialisableSecretStillResolves(t *testing.T) {
	const value = "declared-value-0123456789abcdef"
	g := New([]Secret{{Name: "declared", Value: value, Hosts: []string{"api.example"}}}, DefaultConfig())
	ph := PlaceholderForName("declared")
	if !g.Materializes() {
		t.Fatal("Materializes = false for a declared secret")
	}
	if got := g.Materialize("x " + ph); !strings.Contains(got, value) {
		t.Error("Materialize no longer resolves a declared secret")
	}
	if got := g.MaterializeForHost("x "+ph, "api.example"); !strings.Contains(got, value) {
		t.Error("MaterializeForHost no longer resolves a declared secret toward its host")
	}
	if got := g.MaterializeForHost("x "+ph, "other.example"); strings.Contains(got, value) {
		t.Error("MaterializeForHost resolved a declared secret toward a host outside its scope")
	}
}

// A value both registered RedactOnly and declared redacts to the declared
// placeholder, in either registration order: what an agent reads back must
// resolve when the workflow gave the value to it.
func TestSharedValueRedactsToTheMaterialisablePlaceholder(t *testing.T) {
	const value = "shared-value-0123456789abcdef"
	ambient := Secret{Name: "ambient", Value: value, RedactOnly: true}
	declared := Secret{Name: "declared", Value: value}
	for name, order := range map[string][]Secret{
		"redact-only first": {ambient, declared},
		"declared first":    {declared, ambient},
	} {
		t.Run(name, func(t *testing.T) {
			g := New(order, DefaultConfig())
			red := g.Redact(value)
			if red != PlaceholderForName("declared") {
				t.Fatalf("Redact = %q, want %s", red, PlaceholderForName("declared"))
			}
			if got := g.Materialize(red); got != value {
				t.Errorf("Materialize(%s) = %q, want the declared value back", red, got)
			}
		})
	}
}

// Two entries that share a name keep their own egress scope: a declared
// secret's DLP survives a server-minted credential registered under the same
// name, and the minted value inherits nothing from it.
func TestEgressScopeIsPerEntryNotPerName(t *testing.T) {
	const declared = "declared-value-0123456789abcdef"
	const minted = "minted-value-0123456789abcdef01"
	g := New([]Secret{
		{Name: "forge_publish_token", Value: declared, Hosts: []string{"api.example"}},
		{Name: "forge_publish_token", Value: minted, RedactOnly: true},
	}, DefaultConfig())
	if !g.ExfiltratesTo("x="+declared, "evil.example") {
		t.Error("the declared value may leave toward a host outside its scope")
	}
	if g.ExfiltratesTo("x="+minted, "iterion.example") {
		t.Error("the minted value inherited the declared secret's host scope")
	}
}

// A RedactOnly value is never handed out as a file either.
func TestRedactOnlySecretIsNeverAFile(t *testing.T) {
	const value = "redact-only-file-value-0123456789"
	g := New([]Secret{{Name: "ro", Value: value, FilePath: "/run/iterion/secrets/ro", RedactOnly: true}}, DefaultConfig())
	if len(g.SecretFileHints()) != 0 || g.SecretFilePath("ro") != "" || strings.Contains(g.ResolveSecretRef("ro"), "/") {
		t.Fatal("a RedactOnly value is handed out as a file")
	}
	dir := t.TempDir()
	cleanup, err := g.MaterializeHostFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("MaterializeHostFiles wrote %d file(s) for a RedactOnly value", len(entries))
	}
}

// A workflow may declare a secret under the name a RedactOnly value already
// carries. Each keeps its own placeholder: the declared one resolves to the
// declared value, and the RedactOnly one resolves nowhere — an agent reading
// a placeholder back is never handed the other credential.
func TestARedactOnlyValueNeverSharesADeclaredSecretsPlaceholder(t *testing.T) {
	const minted, declared = "minted-grant-value-1997-xyz", "declared-secret-value-1997-xyz"
	g := New([]Secret{
		{Name: "forge_publish_token", Value: minted, RedactOnly: true},
		{Name: "forge_publish_token", Value: declared},
	}, DefaultConfig())

	redactedMinted := g.Redact(minted)
	if strings.Contains(redactedMinted, minted) {
		t.Fatalf("the RedactOnly value is not redacted: %q", redactedMinted)
	}
	if got := g.Materialize(redactedMinted); got != redactedMinted {
		t.Errorf("the RedactOnly value's placeholder resolves: %q -> %q", redactedMinted, got)
	}
	if got := g.Materialize(redactedMinted); strings.Contains(got, declared) {
		t.Error("reading the RedactOnly placeholder back hands out the DECLARED secret")
	}
	if got := g.Materialize(g.Redact(declared)); got != declared {
		t.Errorf("the declared secret's placeholder no longer resolves: %q", got)
	}
}
