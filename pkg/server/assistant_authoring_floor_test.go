package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The assistant's authoring commit writes a cloud bot's files through the
// same floor guard as every push route: an edit that introduces a
// `contract` into a bot whose manifest declares no floor is refused by name
// and nothing is stored — with the floor declared, it lands.
func TestAuthoringCommitRefusesASyntaxFloorGap(t *testing.T) {
	const teamID = "team-1"
	pinServerBuild(t, "v"+parser.ContractSince+"+deadbeef")
	botStore := botsource.NewMemoryStore()
	created, err := botStore.Create(store.WithTenant(t.Context(), teamID), botsource.BotSource{
		TenantID: teamID,
		Slug:     "demo",
		Files: map[string]string{
			"main.bot": "workflow main:\n  entry: done\n",
			"manifest.yaml": `schema_version: 1
name: demo
authoring:
  editable_files:
    - {scope: bundle, path: main.bot}
    - {scope: bundle, path: manifest.yaml}
`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{botSources: botStore}
	ctx := auth.WithIdentity(t.Context(), auth.Identity{UserID: "u", TeamID: teamID, Role: identity.RoleConfigEditor})
	call := func(endpoint string, payload any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", endpoint, bytes.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		if endpoint == "/snapshot" {
			s.handleAuthoringSnapshot(rec, req)
		} else {
			s.handleAuthoringCommit(rec, req)
		}
		return rec
	}
	editorPath := "botsource://team-1/demo/main.bot"
	snapshot := func() map[string]authoringFileSnapshot {
		rec := call("/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
		if rec.Code != 200 {
			t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
		}
		var snap authoringSnapshotResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		files := map[string]authoringFileSnapshot{}
		for _, f := range snap.Files {
			files[f.Path] = f
		}
		return files
	}
	files := snapshot()
	const contracted = "contract pub:\n  version: 1\n\nworkflow main:\n  contract: pub\n  entry: done\n"
	edit := authoringFileChange{Scope: "bundle", Path: "main.bot", ExpectedSHA256: files["main.bot"].SHA256,
		Replacements: []authoringReplacement{{Before: "workflow main:\n  entry: done\n", After: contracted}}}
	rec := call("/commit", authoringChangeRequest{EditorPath: editorPath, Version: created.Version, Changes: []authoringFileChange{edit}})
	if rec.Code/100 == 2 || !strings.Contains(rec.Body.String(), "bot_engine_floor") || !strings.Contains(rec.Body.String(), parser.ContractSince) {
		t.Fatalf("commit without a floor = %d %s, want a refusal naming the floor", rec.Code, rec.Body.String())
	}
	stored, err := botStore.GetBySlug(store.WithTenant(t.Context(), teamID), teamID, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.Files["main.bot"], "contract pub:") || stored.Version != created.Version {
		t.Fatalf("the refused edit was stored (version %d):\n%s", stored.Version, stored.Files["main.bot"])
	}

	// The same edit with the floor declared in the same commit lands.
	files = snapshot()
	manifest := authoringFileChange{Scope: "bundle", Path: "manifest.yaml", ExpectedSHA256: files["manifest.yaml"].SHA256,
		Replacements: []authoringReplacement{{Before: "name: demo\n", After: "name: demo\nrequires:\n  iterion: \">= " + parser.ContractSince + "\"\n"}}}
	edit.ExpectedSHA256 = files["main.bot"].SHA256
	rec = call("/commit", authoringChangeRequest{EditorPath: editorPath, Version: created.Version, Changes: []authoringFileChange{edit, manifest}})
	if rec.Code/100 != 2 {
		t.Fatalf("commit with the floor = %d %s, want success", rec.Code, rec.Body.String())
	}
	stored, err = botStore.GetBySlug(store.WithTenant(t.Context(), teamID), teamID, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.Files["main.bot"], "contract pub:") {
		t.Fatalf("the accepted edit was not stored:\n%s", stored.Files["main.bot"])
	}
}

// The same commit holds an edit against the REQUIRES half of the floor: an
// edit raising requires.iterion above the deployment's engine floor is
// refused with the same bot_engine_floor code and nothing stored — the
// assistant's write door lands nothing the push routes refuse. At or below
// the floor, the same edit lands.
func TestAuthoringCommitRefusesARequirementAboveTheFloor(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, editor, _ := newBotSourceTestServer(t)
	ctx := auth.WithIdentity(t.Context(), editor)
	manifest := `schema_version: 1
name: rf
requires:
  iterion: ">= 3.112.14"
authoring:
  editable_files:
    - {scope: bundle, path: main.bot}
    - {scope: bundle, path: manifest.yaml}
`
	if _, err := s.botSources.Create(store.WithTenant(t.Context(), "t1"), botsource.BotSource{
		TenantID: "t1",
		Slug:     "rf",
		Files: map[string]string{
			botsource.MainBotFile: "workflow main:\n  entry: done\n",
			"manifest.yaml":       manifest,
		},
	}); err != nil {
		t.Fatal(err)
	}
	const editorPath = "botsource://t1/rf/manifest.yaml"
	snapshot := func() (authoringFileSnapshot, int) {
		body, _ := json.Marshal(authoringSnapshotRequest{EditorPath: editorPath})
		req := httptest.NewRequest("POST", "/api/v1/assistant/authoring/snapshot", bytes.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		s.handleAuthoringSnapshot(rec, req)
		if rec.Code != 200 {
			t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
		}
		var snap authoringSnapshotResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		for _, f := range snap.Files {
			if f.Path == "manifest.yaml" {
				return f, snap.Version
			}
		}
		t.Fatal("snapshot names no manifest.yaml")
		return authoringFileSnapshot{}, 0
	}
	commit := func(raiseTo string) *httptest.ResponseRecorder {
		snap, version := snapshot()
		edit := authoringFileChange{Scope: "bundle", Path: "manifest.yaml", ExpectedSHA256: snap.SHA256,
			Replacements: []authoringReplacement{{Before: `">= 3.112.14"`, After: `"` + raiseTo + `"`}}}
		body, _ := json.Marshal(authoringChangeRequest{EditorPath: editorPath, Version: version, Changes: []authoringFileChange{edit}})
		req := httptest.NewRequest("POST", "/api/v1/assistant/authoring/commit", bytes.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		s.handleAuthoringCommit(rec, req)
		return rec
	}

	// An edit raising the requirement above the floor is refused with the
	// code the push routes' refusal carries, and nothing is stored.
	rec := commit(">= 999.0.0")
	if rec.Code/100 == 2 || !strings.Contains(rec.Body.String(), "bot_engine_floor") || !strings.Contains(rec.Body.String(), "999.0.0") {
		t.Fatalf("commit raising requires above the floor = %d %s, want a bot_engine_floor refusal naming the requirement", rec.Code, rec.Body.String())
	}
	stored, err := s.botSources.GetBySlug(store.WithTenant(t.Context(), "t1"), "t1", "rf")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.Files["manifest.yaml"], "999.0.0") {
		t.Fatalf("the refused edit was stored:\n%s", stored.Files["manifest.yaml"])
	}

	// The push routes refuse the same edit — the doors stay consistent.
	now, id, version, digests := getBundle(t, s, ctx, "rf")
	raised := strings.Replace(now["manifest.yaml"], `">= 3.112.14"`, `">= 999.0.0"`, 1)
	if rec := putFileWith(t, s, ctx, "rf", "manifest.yaml", raised, id, version, digests["manifest.yaml"]); rec.Code != http.StatusConflict {
		t.Fatalf("the same edit through the per-file PUT = %d %s, want 409", rec.Code, rec.Body.String())
	}

	// At the floor, the same edit lands.
	rec = commit(">= 3.116.4")
	if rec.Code/100 != 2 {
		t.Fatalf("commit at the floor = %d %s, want success", rec.Code, rec.Body.String())
	}
	stored, err = s.botSources.GetBySlug(store.WithTenant(t.Context(), "t1"), "t1", "rf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.Files["manifest.yaml"], ">= 3.116.4") {
		t.Fatalf("the accepted edit was not stored:\n%s", stored.Files["manifest.yaml"])
	}
}
