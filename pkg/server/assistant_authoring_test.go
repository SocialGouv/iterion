package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

func authoringFixture(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	bundleDir := filepath.Join(root, "project-bot")
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundleDir, "subbots"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(bundleDir, "main.bot"):            "workflow main:\n  entry: done\n",
		filepath.Join(bundleDir, "subbots", "work.bot"): "workflow work:\n  entry: done\n",
		filepath.Join(root, "scripts", "helper.py"):     "def answer():\n    return 41\n",
		filepath.Join(bundleDir, "manifest.yaml"): `schema_version: 1
name: project-bot
authoring:
  editable_files:
    - scope: bundle
      path: subbots/work.bot
    - scope: workspace
      path: scripts/helper.py
`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{cfg: Config{WorkDir: root}}, filepath.ToSlash(filepath.Join("project-bot", "main.bot"))
}

func TestAuthoringCloudBotSourceUsesVersionCAS(t *testing.T) {
	const teamID = "team-1"
	botStore := botsource.NewMemoryStore()
	created, err := botStore.Create(store.WithTenant(t.Context(), teamID), botsource.BotSource{
		TenantID: teamID,
		Slug:     "demo",
		Files: map[string]string{
			"main.bot":  "workflow main:\n  entry: done\n",
			"helper.py": "value = 1\n",
			"manifest.yaml": `schema_version: 1
name: demo
authoring:
  editable_files:
    - {scope: bundle, path: helper.py}
`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{botSources: botStore}
	ctx := auth.WithIdentity(t.Context(), auth.Identity{UserID: "user-1", TeamID: teamID, Role: identity.RoleConfigEditor})
	call := func(endpoint string, payload any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", endpoint, bytes.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		switch endpoint {
		case "/snapshot":
			s.handleAuthoringSnapshot(rec, req)
		case "/commit":
			s.handleAuthoringCommit(rec, req)
		}
		return rec
	}
	editorPath := "botsource://team-1/demo/main.bot"
	snapshotRec := call("/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if snapshotRec.Code != 200 {
		t.Fatalf("snapshot: %d %s", snapshotRec.Code, snapshotRec.Body.String())
	}
	var snapshot authoringSnapshotResponse
	_ = json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot)
	if snapshot.Version != created.Version || len(snapshot.Files) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	change := authoringFileChange{Scope: "bundle", Path: "helper.py", ExpectedSHA256: snapshot.Files[0].SHA256, Replacements: []authoringReplacement{{Before: "1", After: "2"}}}
	stale := call("/commit", authoringChangeRequest{EditorPath: editorPath, Version: created.Version + 1, Changes: []authoringFileChange{change}})
	if stale.Code != 409 {
		t.Fatalf("stale = %d %s", stale.Code, stale.Body.String())
	}
	saved := call("/commit", authoringChangeRequest{EditorPath: editorPath, Version: created.Version, Changes: []authoringFileChange{change}})
	if saved.Code != 200 {
		t.Fatalf("commit = %d %s", saved.Code, saved.Body.String())
	}
	got, err := botStore.GetBySlug(store.WithTenant(t.Context(), teamID), teamID, "demo")
	if err != nil || got.Files["helper.py"] != "value = 2\n" || got.Version != created.Version+1 {
		t.Fatalf("stored = %#v, err=%v", got, err)
	}
}

func authoringCall(t *testing.T, s *Server, endpoint string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", endpoint, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	switch endpoint {
	case "/snapshot":
		s.handleAuthoringSnapshot(rec, req)
	case "/preview":
		s.handleAuthoringPreview(rec, req)
	case "/commit":
		s.handleAuthoringCommit(rec, req)
	default:
		t.Fatalf("unknown endpoint %s", endpoint)
	}
	return rec
}

func TestAuthoringSnapshotPreviewAndCommit(t *testing.T) {
	s, editorPath := authoringFixture(t)
	snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if snapshotRec.Code != 200 {
		t.Fatalf("snapshot: %d %s", snapshotRec.Code, snapshotRec.Body.String())
	}
	var snapshot authoringSnapshotResponse
	if err := json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 2 || !snapshot.Files[1].Available || snapshot.Files[1].SHA256 == "" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	change := authoringFileChange{
		Scope:          "workspace",
		Path:           "scripts/helper.py",
		ExpectedSHA256: snapshot.Files[1].SHA256,
		Replacements:   []authoringReplacement{{Before: "return 41", After: "return 42"}},
	}
	previewRec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}})
	if previewRec.Code != 200 || !strings.Contains(previewRec.Body.String(), "return 42") {
		t.Fatalf("preview: %d %s", previewRec.Code, previewRec.Body.String())
	}
	before, _ := os.ReadFile(filepath.Join(s.cfg.WorkDir, "scripts", "helper.py"))
	if strings.Contains(string(before), "42") {
		t.Fatal("preview wrote the file")
	}
	commitRec := authoringCall(t, s, "/commit", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}})
	if commitRec.Code != 200 {
		t.Fatalf("commit: %d %s", commitRec.Code, commitRec.Body.String())
	}
	after, _ := os.ReadFile(filepath.Join(s.cfg.WorkDir, "scripts", "helper.py"))
	if !strings.Contains(string(after), "return 42") {
		t.Fatalf("file = %q", after)
	}
}

func TestAuthoringRejectsOutOfScopeBeforeReadAndStaleHash(t *testing.T) {
	s, editorPath := authoringFixture(t)
	outside := authoringFileChange{
		Scope: "workspace", Path: "secret.txt", ExpectedSHA256: "made-up",
		Replacements: []authoringReplacement{{Before: "secret", After: "stolen"}},
	}
	rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{outside}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "outside authoring.editable_files") {
		t.Fatalf("outside: %d %s", rec.Code, rec.Body.String())
	}
	stale := outside
	stale.Path = "scripts/helper.py"
	rec = authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{stale}})
	if rec.Code != 409 {
		t.Fatalf("stale: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthoringRejectsAmbiguousReplacementAndBrokenSubbot(t *testing.T) {
	s, editorPath := authoringFixture(t)
	snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	var snapshot authoringSnapshotResponse
	_ = json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot)
	pyHash := snapshot.Files[1].SHA256
	ambiguous := authoringFileChange{Scope: "workspace", Path: "scripts/helper.py", ExpectedSHA256: pyHash, Replacements: []authoringReplacement{{Before: " ", After: "  "}}}
	rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{ambiguous}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "want exactly once") {
		t.Fatalf("ambiguous: %d %s", rec.Code, rec.Body.String())
	}
	botHash := snapshot.Files[0].SHA256
	broken := authoringFileChange{Scope: "bundle", Path: "subbots/work.bot", ExpectedSHA256: botHash, Replacements: []authoringReplacement{{Before: "workflow work:", After: "this is not a workflow:"}}}
	rec = authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{broken}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "does not compile") {
		t.Fatalf("broken bot: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthoringRejectsSymlinkOutsideDeclaredRoot(t *testing.T) {
	s, editorPath := authoringFixture(t)
	external := t.TempDir()
	secret := filepath.Join(external, "secret.py")
	if err := os.WriteFile(secret, []byte("token = 'private'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.cfg.WorkDir, "scripts", "link.py")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(s.cfg.WorkDir, "project-bot", "manifest.yaml")
	body := `schema_version: 1
name: project-bot
authoring:
  editable_files:
    - {scope: workspace, path: scripts/link.py}
`
	if err := os.WriteFile(manifest, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "escapes") {
		t.Fatalf("symlink escape: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthoringChangeShapeLimitsAreEnforced(t *testing.T) {
	limits := authoringLimits{
		maxFiles: 1, maxPerFile: 1, maxReplacements: 1,
		maxBlockBytes: 4, maxTotalBytes: 8,
	}
	change := authoringFileChange{
		Scope: "bundle", Path: "a.py",
		Replacements: []authoringReplacement{{Before: "a", After: "b"}},
	}
	if err := validateAuthoringChangesShape([]authoringFileChange{change}, limits); err != nil {
		t.Fatalf("valid shape: %v", err)
	}
	if err := validateAuthoringChangesShape([]authoringFileChange{change, {
		Scope: "bundle", Path: "b.py",
		Replacements: []authoringReplacement{{Before: "a", After: "b"}},
	}}, limits); err == nil || !strings.Contains(err.Error(), "file limit") {
		t.Fatalf("file limit error = %v", err)
	}
	change.Replacements[0].After = "12345"
	if err := validateAuthoringChangesShape([]authoringFileChange{change}, limits); err == nil || !strings.Contains(err.Error(), "block limit") {
		t.Fatalf("block limit error = %v", err)
	}
}

func TestAuthoringCompilesWorkspaceBotChanges(t *testing.T) {
	s, editorPath := authoringFixture(t)
	workspaceBot := filepath.Join(s.cfg.WorkDir, "scripts", "helper.bot")
	if err := os.WriteFile(workspaceBot, []byte("workflow helper:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(s.cfg.WorkDir, "project-bot", "manifest.yaml")
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("    - {scope: workspace, path: scripts/helper.bot}\n")...)
	if err := os.WriteFile(manifest, body, 0o644); err != nil {
		t.Fatal(err)
	}

	snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	var snapshot authoringSnapshotResponse
	if err := json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	var hash string
	for _, file := range snapshot.Files {
		if file.Path == "scripts/helper.bot" {
			hash = file.SHA256
		}
	}
	if hash == "" {
		t.Fatalf("workspace bot missing from snapshot: %#v", snapshot.Files)
	}
	broken := authoringFileChange{
		Scope: "workspace", Path: "scripts/helper.bot", ExpectedSHA256: hash,
		Replacements: []authoringReplacement{{Before: "workflow helper:", After: "not a workflow:"}},
	}
	rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{broken}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "does not compile") {
		t.Fatalf("broken workspace bot: %d %s", rec.Code, rec.Body.String())
	}
}
