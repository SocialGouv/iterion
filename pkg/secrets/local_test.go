package secrets

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sealedRec(t *testing.T, sealer Sealer, name, value string, hosts ...string) GenericSecret {
	t.Helper()
	id := NewGenericSecretID()
	sealed, err := SealGenericSecret(sealer, id, []byte(value))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return GenericSecret{
		ID:           id,
		ScopeTeamID:  LocalScopeTeam,
		Name:         name,
		Last4:        Last4(value),
		SealedSecret: sealed,
		CreatedAt:    time.Now().UTC(),
		Fingerprint:  FingerprintSHA256(value),
		AllowedHosts: hosts,
	}
}

func TestFileStore_RoundTripSealedAndResolve(t *testing.T) {
	sealer := newTestSealer(t)
	path := filepath.Join(t.TempDir(), "secrets.json")
	st, err := NewFileGenericSecretStore(path)
	if err != nil {
		t.Fatalf("NewFileGenericSecretStore: %v", err)
	}
	ctx := context.Background()
	if err := st.Create(ctx, sealedRec(t, sealer, "GITHUB_TOKEN", "ghp_topsecret_1234", "github.com")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// File must NOT contain the plaintext.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if bytes.Contains(raw, []byte("ghp_topsecret_1234")) {
		t.Fatalf("plaintext leaked into sealed store file")
	}

	// Reopen from disk and resolve.
	st2, err := NewFileGenericSecretStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	res, err := ResolveGeneric(ctx, st2, LocalScopeTeam, "", []string{"GITHUB_TOKEN"}, sealer, nil)
	if err != nil {
		t.Fatalf("ResolveGeneric: %v", err)
	}
	got, ok := res["GITHUB_TOKEN"]
	if !ok {
		t.Fatalf("secret not resolved")
	}
	if string(got.Plaintext) != "ghp_topsecret_1234" {
		t.Fatalf("plaintext = %q, want ghp_topsecret_1234", got.Plaintext)
	}
	if len(got.AllowedHosts) != 1 || got.AllowedHosts[0] != "github.com" {
		t.Fatalf("AllowedHosts = %v, want [github.com]", got.AllowedHosts)
	}
}

func TestLayered_ProjectOverridesGlobal(t *testing.T) {
	sealer := newTestSealer(t)
	dir := t.TempDir()
	global, _ := NewFileGenericSecretStore(filepath.Join(dir, "global.json"))
	project, _ := NewFileGenericSecretStore(filepath.Join(dir, "project.json"))
	ctx := context.Background()

	_ = global.Create(ctx, sealedRec(t, sealer, "SHARED", "global_value_xxxx"))
	_ = global.Create(ctx, sealedRec(t, sealer, "GLOBAL_ONLY", "gonly_value_xxxx"))
	_ = project.Create(ctx, sealedRec(t, sealer, "SHARED", "project_value_yyyy"))

	layered := NewLayeredGenericSecretStore(global, project)

	res, err := ResolveGeneric(ctx, layered, LocalScopeTeam, "", []string{"SHARED", "GLOBAL_ONLY"}, sealer, nil)
	if err != nil {
		t.Fatalf("ResolveGeneric: %v", err)
	}
	if string(res["SHARED"].Plaintext) != "project_value_yyyy" {
		t.Fatalf("SHARED = %q, want project_value_yyyy (project overrides global)", res["SHARED"].Plaintext)
	}
	if string(res["GLOBAL_ONLY"].Plaintext) != "gonly_value_xxxx" {
		t.Fatalf("GLOBAL_ONLY = %q, want gonly_value_xxxx", res["GLOBAL_ONLY"].Plaintext)
	}

	// ListScoped tags each with its owning layer.
	scoped, err := layered.ListScoped(ctx, LocalScopeTeam, "")
	if err != nil {
		t.Fatalf("ListScoped: %v", err)
	}
	byName := map[string]string{}
	for _, sc := range scoped {
		byName[sc.Secret.Name] = sc.Scope
	}
	if byName["SHARED"] != "project" {
		t.Fatalf("SHARED scope = %q, want project", byName["SHARED"])
	}
	if byName["GLOBAL_ONLY"] != "global" {
		t.Fatalf("GLOBAL_ONLY scope = %q, want global", byName["GLOBAL_ONLY"])
	}
}

func TestFileStore_GetByNameAndDelete(t *testing.T) {
	sealer := newTestSealer(t)
	st, _ := NewFileGenericSecretStore(filepath.Join(t.TempDir(), "s.json"))
	ctx := context.Background()
	rec := sealedRec(t, sealer, "TOKEN", "value_abcd_1234")
	_ = st.Create(ctx, rec)

	got, ok := st.GetByName("TOKEN")
	if !ok || got.ID != rec.ID {
		t.Fatalf("GetByName miss")
	}
	if err := st.Delete(ctx, rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := st.GetByName("TOKEN"); ok {
		t.Fatalf("secret still present after delete")
	}
}

func TestResolveLocalCredentials(t *testing.T) {
	sealer := newTestSealer(t)
	st, _ := NewFileGenericSecretStore(filepath.Join(t.TempDir(), "s.json"))
	ctx := context.Background()
	_ = st.Create(ctx, sealedRec(t, sealer, "API_KEY", "sk_live_abcd1234", "api.example.com"))

	creds, err := ResolveLocalCredentials(ctx, st, sealer, []string{"API_KEY", "MISSING"}, nil)
	if err != nil {
		t.Fatalf("ResolveLocalCredentials: %v", err)
	}
	if creds.Generic["API_KEY"] != "sk_live_abcd1234" {
		t.Fatalf("Generic[API_KEY] = %q", creds.Generic["API_KEY"])
	}
	if _, ok := creds.Generic["MISSING"]; ok {
		t.Fatalf("MISSING should not resolve")
	}
	if got := creds.GenericHosts["API_KEY"]; len(got) != 1 || got[0] != "api.example.com" {
		t.Fatalf("GenericHosts[API_KEY] = %v", got)
	}
}

func TestFileStore_UpsertByName(t *testing.T) {
	sealer := newTestSealer(t)
	st, _ := NewFileGenericSecretStore(filepath.Join(t.TempDir(), "s.json"))

	// Create.
	rec, created, err := st.UpsertByName(sealer, "TOKEN", "value_one_1111", []string{"a.com"}, true)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if rec.Last4 != "1111" || len(rec.AllowedHosts) != 1 {
		t.Fatalf("bad create rec: %+v", rec)
	}
	firstID := rec.ID

	// Rotate WITHOUT applyHosts → value changes, ID stable, hosts preserved.
	rec, created, err = st.UpsertByName(sealer, "TOKEN", "value_two_2222", nil, false)
	if err != nil || created {
		t.Fatalf("rotate: created=%v err=%v", created, err)
	}
	if rec.ID != firstID {
		t.Fatalf("rotate changed ID: %s != %s", rec.ID, firstID)
	}
	if rec.Last4 != "2222" {
		t.Fatalf("rotate last4 = %q, want 2222", rec.Last4)
	}
	if len(rec.AllowedHosts) != 1 || rec.AllowedHosts[0] != "a.com" {
		t.Fatalf("rotate should PRESERVE hosts, got %v", rec.AllowedHosts)
	}

	// Rotate WITH applyHosts=true and empty hosts → hosts cleared.
	rec, _, err = st.UpsertByName(sealer, "TOKEN", "value_three_3333", nil, true)
	if err != nil {
		t.Fatalf("rotate clear: %v", err)
	}
	if len(rec.AllowedHosts) != 0 {
		t.Fatalf("applyHosts=true with nil should clear, got %v", rec.AllowedHosts)
	}

	// Still exactly one record (no duplicate).
	all, _ := st.ListByTeam(context.Background(), LocalScopeTeam, "")
	if len(all) != 1 {
		t.Fatalf("expected 1 record after upserts, got %d", len(all))
	}
}

func TestValidGenericSecretName(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"GITHUB_TOKEN", true},
		{"a", true},
		{"_x", true},
		{"X1", true},
		{"1X", false},
		{"has-dash", false},
		{"has space", false},
		{"", false},
	} {
		if got := ValidGenericSecretName(c.name); got != c.want {
			t.Errorf("ValidGenericSecretName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
