package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
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

func authoringBootstrapFixture(t *testing.T, manifestName string) (*Server, string, string) {
	t.Helper()
	root := t.TempDir()
	bundleDir := filepath.Join(root, "project-bot")
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema_version: 1\nname: project-bot\n"
	files := map[string]string{
		filepath.Join(bundleDir, "main.bot"):        "workflow main:\n  entry: done\n",
		filepath.Join(bundleDir, manifestName):      manifest,
		filepath.Join(root, "scripts", "helper.py"): "def answer():\n    return 41\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{cfg: Config{WorkDir: root}}, filepath.ToSlash(filepath.Join("project-bot", "main.bot")), manifest
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
    - {scope: bundle, path: main.bot}
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
	if snapshot.Version != created.Version || len(snapshot.Files) != 2 || snapshot.ActiveFile == nil || snapshot.ActiveFile.Scope != bundle.AuthoringScopeBundle || snapshot.ActiveFile.Path != "main.bot" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	var helper authoringFileSnapshot
	for _, file := range snapshot.Files {
		if file.Scope == bundle.AuthoringScopeBundle && file.Path == "helper.py" {
			helper = file
			break
		}
	}
	change := authoringFileChange{Scope: "bundle", Path: "helper.py", ExpectedSHA256: helper.SHA256, Replacements: []authoringReplacement{{Before: "1", After: "2"}}}
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

func TestAuthoringCloudBotWithoutDeclaredFilesExposesOnlyActiveFile(t *testing.T) {
	const teamID = "team-1"
	botStore := botsource.NewMemoryStore()
	created, err := botStore.Create(store.WithTenant(t.Context(), teamID), botsource.BotSource{
		TenantID: teamID,
		Slug:     "active-only",
		Files: map[string]string{
			"main.bot":      "workflow main:\n  entry: done\n",
			"helper.py":     "value = 1\n",
			"manifest.yaml": "schema_version: 1\nname: active-only\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{botSources: botStore}
	editorPath := "botsource://team-1/active-only/main.bot"

	request := func(ctx context.Context) *httptest.ResponseRecorder {
		body, _ := json.Marshal(authoringSnapshotRequest{EditorPath: editorPath})
		req := httptest.NewRequest("POST", "/snapshot", bytes.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		s.handleAuthoringSnapshot(rec, req)
		return rec
	}
	if rec := request(t.Context()); rec.Code != 403 {
		t.Fatalf("unauthorized snapshot: %d %s", rec.Code, rec.Body.String())
	}
	ctx := auth.WithIdentity(t.Context(), auth.Identity{UserID: "user-1", TeamID: teamID, Role: identity.RoleConfigEditor})
	rec := request(ctx)
	if rec.Code != 200 {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	var snapshot authoringSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != created.Version || len(snapshot.Files) != 1 || snapshot.Files[0].Path != "main.bot" || snapshot.Files[0].GitCommittable || snapshot.Files[0].Readable || snapshot.ActiveFile == nil || snapshot.ActiveFile.Path != "main.bot" {
		t.Fatalf("active-only cloud snapshot = %#v", snapshot)
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
	case "/git-commit":
		s.handleAuthoringGitCommit(rec, req)
	case "/git-push":
		s.handleAuthoringGitPublish(rec, req)
	default:
		t.Fatalf("unknown endpoint %s", endpoint)
	}
	return rec
}

func TestAuthoringGitPublishPinsCurrentHeadToFreshOriginBranch(t *testing.T) {
	s, editorPath := authoringFixture(t)
	initAuthoringGit(t, s.cfg.WorkDir)
	bare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := gittest.Cmd("", "init", "--bare", "--quiet", bare).CombinedOutput(); err != nil {
		t.Fatalf("init bare origin: %v\n%s", err, out)
	}
	if _, err := runAuthoringGit(t.Context(), s.cfg.WorkDir, "remote", "add", "origin", bare); err != nil {
		t.Fatal(err)
	}
	head, err := runAuthoringGit(t.Context(), s.cfg.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(head)
	request := authoringGitPublishRequest{EditorPath: editorPath, Commit: head[:12], Branch: "shared-planner/v0.3.1"}
	if rec := authoringCall(t, s, "/git-push", request); rec.Code != 200 {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body.String())
	}
	remote, err := gittest.Cmd("", "--git-dir", bare, "rev-parse", "refs/heads/shared-planner/v0.3.1").CombinedOutput()
	if err != nil || strings.TrimSpace(string(remote)) != head {
		t.Fatalf("remote branch = %q, err=%v, want %s", remote, err, head)
	}
	if rec := authoringCall(t, s, "/git-push", request); rec.Code != 409 {
		t.Fatalf("existing branch = %d %s", rec.Code, rec.Body.String())
	}
	if rec := authoringCall(t, s, "/git-push", authoringGitPublishRequest{EditorPath: editorPath, Commit: "deadbeefdead", Branch: "other"}); rec.Code != 409 {
		t.Fatalf("mismatched head = %d %s", rec.Code, rec.Body.String())
	}
	if rec := authoringCall(t, s, "/git-push", authoringGitPublishRequest{EditorPath: editorPath, Commit: head[:12], Branch: "bad:ref"}); rec.Code != 400 {
		t.Fatalf("malformed branch = %d %s", rec.Code, rec.Body.String())
	}
}

func initAuthoringGit(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "Authoring Test"},
		{"config", "user.email", "authoring@example.invalid"},
		{"add", "--", "."},
		{"commit", "-qm", "initial"},
	} {
		if _, err := runAuthoringGit(t.Context(), root, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuthoringGitCommitIsPerimeterAndIndexBounded(t *testing.T) {
	t.Run("commits exactly selected current files", func(t *testing.T) {
		s, editorPath := authoringFixture(t)
		initAuthoringGit(t, s.cfg.WorkDir)
		botPath := filepath.Join(s.cfg.WorkDir, "project-bot", "subbots", "work.bot")
		helperPath := filepath.Join(s.cfg.WorkDir, "scripts", "helper.py")
		if err := os.WriteFile(botPath, []byte("workflow work:\n  entry: published\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(helperPath, []byte("def answer():\n    return 42\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
		var snapshot authoringSnapshotResponse
		if snapshotRec.Code != 200 || json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot) != nil {
			t.Fatalf("snapshot: %d %s", snapshotRec.Code, snapshotRec.Body.String())
		}
		files := make([]authoringGitCommitFile, 0, len(snapshot.Files))
		for _, file := range snapshot.Files {
			if !file.GitCommittable {
				continue
			}
			files = append(files, authoringGitCommitFile{Scope: file.Scope, Path: file.Path, ExpectedSHA256: file.SHA256})
		}
		rec := authoringCall(t, s, "/git-commit", authoringGitCommitRequest{
			EditorPath: editorPath,
			Files:      files,
			Message:    "chore(test): commit declared files",
		})
		if rec.Code != 200 {
			t.Fatalf("git commit: %d %s", rec.Code, rec.Body.String())
		}
		changed, err := runAuthoringGit(t.Context(), s.cfg.WorkDir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
		if err != nil || !sameGitPathSet(splitGitLines(changed), []string{"project-bot/subbots/work.bot", "scripts/helper.py"}) {
			t.Fatalf("committed paths = %q, err=%v", changed, err)
		}
	})

	t.Run("rejects stale content and unrelated staged changes", func(t *testing.T) {
		s, editorPath := authoringFixture(t)
		initAuthoringGit(t, s.cfg.WorkDir)
		helperPath := filepath.Join(s.cfg.WorkDir, "scripts", "helper.py")
		if err := os.WriteFile(helperPath, []byte("def answer():\n    return 42\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
		var snapshot authoringSnapshotResponse
		_ = json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot)
		file := snapshot.Files[1]
		if err := os.WriteFile(helperPath, []byte("def answer():\n    return 43\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stale := authoringCall(t, s, "/git-commit", authoringGitCommitRequest{EditorPath: editorPath, Files: []authoringGitCommitFile{{Scope: file.Scope, Path: file.Path, ExpectedSHA256: file.SHA256}}, Message: "chore(test): stale"})
		if stale.Code != 409 {
			t.Fatalf("stale = %d %s", stale.Code, stale.Body.String())
		}
		freshRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
		_ = json.Unmarshal(freshRec.Body.Bytes(), &snapshot)
		file = snapshot.Files[1]
		mainPath := filepath.Join(s.cfg.WorkDir, "project-bot", "main.bot")
		if err := os.WriteFile(mainPath, []byte("workflow main:\n  entry: unrelated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runAuthoringGit(t.Context(), s.cfg.WorkDir, "add", "--", "project-bot/main.bot"); err != nil {
			t.Fatal(err)
		}
		blocked := authoringCall(t, s, "/git-commit", authoringGitCommitRequest{EditorPath: editorPath, Files: []authoringGitCommitFile{{Scope: file.Scope, Path: file.Path, ExpectedSHA256: file.SHA256}}, Message: "chore(test): blocked"})
		if blocked.Code != 400 || !strings.Contains(blocked.Body.String(), "non-selected") {
			t.Fatalf("blocked = %d %s", blocked.Code, blocked.Body.String())
		}
	})
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
	if len(snapshot.Files) != 3 || !snapshot.Files[1].Available || snapshot.Files[1].SHA256 == "" {
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

func TestAuthoringCreatesDeclaredMissingLocalJSONWithoutOverwriting(t *testing.T) {
	s, editorPath := authoringFixture(t)
	manifestPath := filepath.Join(s.cfg.WorkDir, "project-bot", bundle.ManifestFile)
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte("    - {scope: workspace, path: iterion/vertical/planner-input-r4.json}\n")...)
	if err := os.MkdirAll(filepath.Join(s.cfg.WorkDir, "iterion", "vertical"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if snapshotRec.Code != 200 {
		t.Fatalf("snapshot: %d %s", snapshotRec.Code, snapshotRec.Body.String())
	}
	var snapshot authoringSnapshotResponse
	if err := json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	var missing authoringFileSnapshot
	for _, file := range snapshot.Files {
		if file.Path == "iterion/vertical/planner-input-r4.json" {
			missing = file
			break
		}
	}
	if missing.Available || missing.SHA256 != "" || !missing.IsManifestDeclared || missing.Reason != "declared_missing_local_file" {
		t.Fatalf("missing snapshot = %#v", missing)
	}

	change := authoringFileChange{
		Scope:  bundle.AuthoringScopeWorkspace,
		Path:   missing.Path,
		Create: &authoringCreate{Content: "{\"run_id\":\"tabarria-v1-epics-r4\"}\n"},
	}
	previewRec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}})
	if previewRec.Code != 200 {
		t.Fatalf("preview: %d %s", previewRec.Code, previewRec.Body.String())
	}
	var preview authoringChangeResponse
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Files) != 1 || preview.Files[0].Operation != "create" || preview.Files[0].Before != "" || len(preview.Validation.Checks) != 1 || preview.Validation.Checks[0].Kind != "json_syntax" {
		t.Fatalf("preview = %#v", preview)
	}
	destination := filepath.Join(s.cfg.WorkDir, "iterion", "vertical", "planner-input-r4.json")
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview created destination: %v", err)
	}

	commitRec := authoringCall(t, s, "/commit", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}})
	if commitRec.Code != 200 {
		t.Fatalf("commit: %d %s", commitRec.Code, commitRec.Body.String())
	}
	body, err := os.ReadFile(destination)
	if err != nil || string(body) != change.Create.Content {
		t.Fatalf("created file = %q, err=%v", body, err)
	}

	collision := authoringCall(t, s, "/commit", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}})
	if collision.Code != 409 || !strings.Contains(collision.Body.String(), `"error_code":"file_already_exists"`) {
		t.Fatalf("collision: %d %s", collision.Code, collision.Body.String())
	}
	body, err = os.ReadFile(destination)
	if err != nil || string(body) != change.Create.Content {
		t.Fatalf("collision overwrote file = %q, err=%v", body, err)
	}
}

func TestAuthoringCreateRejectsUnsafeOrInvalidTargets(t *testing.T) {
	t.Run("invalid JSON", func(t *testing.T) {
		s, editorPath := authoringFixture(t)
		manifestPath := filepath.Join(s.cfg.WorkDir, "project-bot", bundle.ManifestFile)
		manifest, _ := os.ReadFile(manifestPath)
		manifest = append(manifest, []byte("    - {scope: workspace, path: scripts/new.json}\n")...)
		if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
			t.Fatal(err)
		}
		change := authoringFileChange{Scope: "workspace", Path: "scripts/new.json", Create: &authoringCreate{Content: "{"}}
		rec := authoringCall(t, s, "/commit", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}})
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "json_syntax_failed") {
			t.Fatalf("invalid JSON: %d %s", rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(filepath.Join(s.cfg.WorkDir, "scripts", "new.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid JSON was written: %v", err)
		}
	})

	t.Run("undeclared and active-only", func(t *testing.T) {
		s, editorPath := authoringFixture(t)
		for _, change := range []authoringFileChange{
			{Scope: "workspace", Path: "scripts/new.json", Create: &authoringCreate{Content: "{}"}},
			{Scope: "bundle", Path: "main.bot", Create: &authoringCreate{Content: "workflow main:\n  entry: done\n"}},
		} {
			rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}})
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), "not explicitly declared") {
				t.Fatalf("unsafe target: %d %s", rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("missing parent and symlink", func(t *testing.T) {
		s, editorPath := authoringFixture(t)
		manifestPath := filepath.Join(s.cfg.WorkDir, "project-bot", bundle.ManifestFile)
		manifest, _ := os.ReadFile(manifestPath)
		manifest = append(manifest, []byte("    - {scope: workspace, path: absent/new.json}\n    - {scope: workspace, path: linked/new.json}\n")...)
		if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(s.cfg.WorkDir, "scripts"), filepath.Join(s.cfg.WorkDir, "linked")); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			path string
			want string
		}{
			{path: "absent/new.json", want: "parent directory does not exist"},
			{path: "linked/new.json", want: "symbolic link"},
		} {
			change := authoringFileChange{Scope: "workspace", Path: tc.path, Create: &authoringCreate{Content: "{}"}}
			rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}})
			if rec.Code != 400 && rec.Code != 403 {
				t.Fatalf("%s status: %d %s", tc.path, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("%s: %d %s", tc.path, rec.Code, rec.Body.String())
			}
		}
	})
}

func TestAuthoringCreateCommitRechecksTheLiveManifest(t *testing.T) {
	s, editorPath := authoringFixture(t)
	manifestPath := filepath.Join(s.cfg.WorkDir, "project-bot", bundle.ManifestFile)
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	declared := append(append([]byte(nil), original...), []byte("    - {scope: workspace, path: scripts/new.json}\n")...)
	if err := os.WriteFile(manifestPath, declared, 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/commit", nil)
	target, err := s.resolveAuthoringTarget(req, editorPath)
	if err != nil {
		t.Fatal(err)
	}
	previews, resolved, err := target.preview([]authoringFileChange{{
		Scope: "workspace", Path: "scripts/new.json", Create: &authoringCreate{Content: "{}\n"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.commitAuthoring(req, target, previews, resolved); err == nil || !strings.Contains(err.Error(), "left the live authoring perimeter") {
		t.Fatalf("commit after declaration removal = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.WorkDir, "scripts", "new.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale declaration created file: %v", err)
	}
}

func TestAuthoringRollbackRestoresReplacementsAndRemovesOnlyUnchangedCreates(t *testing.T) {
	s, _ := authoringFixture(t)
	replacedPath := filepath.Join(s.cfg.WorkDir, "scripts", "helper.py")
	createdPath := filepath.Join(s.cfg.WorkDir, "scripts", "created.json")
	before := "def answer():\n    return 41\n"
	after := "def answer():\n    return 42\n"
	created := "{}\n"
	if err := os.WriteFile(replacedPath, []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(createdPath, []byte(created), 0o644); err != nil {
		t.Fatal(err)
	}
	previews := []authoringPreviewFile{
		{Scope: "workspace", Path: "scripts/helper.py", Operation: "replace", Before: before, After: after},
		{Scope: "workspace", Path: "scripts/created.json", Operation: "create", Before: "", After: created},
	}
	resolved := []resolvedAuthoringFile{{abs: replacedPath}, {abs: createdPath}}
	if errs := s.rollbackAuthoring(previews, resolved, []int{0, 1}); len(errs) != 0 {
		t.Fatalf("rollback errors = %v", errs)
	}
	body, err := os.ReadFile(replacedPath)
	if err != nil || string(body) != before {
		t.Fatalf("replacement rollback = %q, err=%v", body, err)
	}
	if _, err := os.Stat(createdPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file survived rollback: %v", err)
	}

	if err := os.WriteFile(createdPath, []byte("changed elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	errs := s.rollbackAuthoring(previews[1:], resolved[1:], []int{0})
	if len(errs) != 1 || !strings.Contains(errs[0], "rollback refused") {
		t.Fatalf("changed create rollback errors = %v", errs)
	}
	body, err = os.ReadFile(createdPath)
	if err != nil || string(body) != "changed elsewhere\n" {
		t.Fatalf("rollback removed foreign content = %q, err=%v", body, err)
	}
}

func TestAuthoringPythonSyntaxValidationIsInMemoryAndFailClosed(t *testing.T) {
	s, editorPath := authoringFixture(t)
	snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	var snapshot authoringSnapshotResponse
	if snapshotRec.Code != 200 || json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot) != nil {
		t.Fatalf("snapshot: %d %s", snapshotRec.Code, snapshotRec.Body.String())
	}
	var helper authoringFileSnapshot
	for _, file := range snapshot.Files {
		if file.Scope == bundle.AuthoringScopeWorkspace && file.Path == "scripts/helper.py" {
			helper = file
			break
		}
	}
	change := func(after string) authoringChangeRequest {
		return authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{{
			Scope: bundle.AuthoringScopeWorkspace, Path: helper.Path, ExpectedSHA256: helper.SHA256,
			Replacements: []authoringReplacement{{Before: "return 41", After: after}},
		}}}
	}
	valid := authoringCall(t, s, "/preview", change("return 42"))
	if valid.Code != 200 {
		t.Fatalf("valid Python preview: %d %s", valid.Code, valid.Body.String())
	}
	var result authoringChangeResponse
	if err := json.Unmarshal(valid.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Validation.BehavioralTests != "not_run" || len(result.Validation.Checks) != 1 || result.Validation.Checks[0].Kind != "python_syntax" || result.Validation.Checks[0].Status != "passed" {
		t.Fatalf("validation = %#v", result.Validation)
	}
	invalid := authoringCall(t, s, "/preview", change("return ("))
	if invalid.Code != 400 || !strings.Contains(invalid.Body.String(), "python_syntax_failed") {
		t.Fatalf("invalid Python preview: %d %s", invalid.Code, invalid.Body.String())
	}
	t.Setenv("ITERION_ASSISTANT_PYTHON", filepath.Join(t.TempDir(), "missing-python"))
	unavailable := authoringCall(t, s, "/preview", change("return 43"))
	if unavailable.Code != 400 || !strings.Contains(unavailable.Body.String(), "python_parser_unavailable") {
		t.Fatalf("unavailable Python parser: %d %s", unavailable.Code, unavailable.Body.String())
	}
	content, err := os.ReadFile(filepath.Join(s.cfg.WorkDir, "scripts", "helper.py"))
	if err != nil || !strings.Contains(string(content), "return 41") {
		t.Fatalf("preview changed workspace file: %q, err=%v", content, err)
	}
}

func TestAuthoringSnapshotAttestsAndReplacesActiveBundleFile(t *testing.T) {
	s, editorPath := authoringFixture(t)
	manifest := filepath.Join(s.cfg.WorkDir, "project-bot", bundle.ManifestFile)
	if err := os.WriteFile(manifest, []byte(`schema_version: 1
name: project-bot
authoring:
  editable_files:
    - {scope: bundle, path: main.bot}
    - {scope: workspace, path: scripts/helper.py}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if rec.Code != 200 {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	var snapshot authoringSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveFile == nil || snapshot.ActiveFile.Scope != bundle.AuthoringScopeBundle || snapshot.ActiveFile.Path != "main.bot" {
		t.Fatalf("active file = %#v", snapshot.ActiveFile)
	}
	if len(snapshot.Files) != 2 || !snapshot.Files[0].GitCommittable {
		t.Fatalf("declared active snapshot = %#v", snapshot)
	}

	manifest = filepath.Join(s.cfg.WorkDir, "project-bot", bundle.ManifestFile)
	if err := os.WriteFile(manifest, []byte(`schema_version: 1
name: project-bot
authoring:
  editable_files:
    - {scope: workspace, path: scripts/helper.py}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if rec.Code != 200 {
		t.Fatalf("snapshot outside perimeter: %d %s", rec.Code, rec.Body.String())
	}
	snapshot = authoringSnapshotResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveFile == nil || snapshot.ActiveFile.Path != "main.bot" || len(snapshot.Files) != 2 {
		t.Fatalf("active replacement snapshot = %#v", snapshot)
	}
	active := snapshot.Files[1]
	if active.Path != "main.bot" || active.GitCommittable || !active.Available || !active.Readable || active.SHA256 == "" {
		t.Fatalf("active replacement file = %#v", active)
	}
	change := authoringFileChange{
		Scope:          bundle.AuthoringScopeBundle,
		Path:           "main.bot",
		ExpectedSHA256: active.SHA256,
		Replacements: []authoringReplacement{{
			Before: "workflow main:\n  entry: done\n",
			After:  "schema active_input:\n  value: string\n\nworkflow main:\n  entry: done\n",
		}},
	}
	if rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "schema active_input") {
		t.Fatalf("active preview: %d %s", rec.Code, rec.Body.String())
	}
	if rec := authoringCall(t, s, "/commit", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}}); rec.Code != 200 {
		t.Fatalf("active commit: %d %s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(filepath.Join(s.cfg.WorkDir, "project-bot", "main.bot"))
	if err != nil || !strings.Contains(string(after), "schema active_input") {
		t.Fatalf("active file = %q, err=%v", after, err)
	}

	initAuthoringGit(t, s.cfg.WorkDir)
	freshRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if freshRec.Code != 200 || json.Unmarshal(freshRec.Body.Bytes(), &snapshot) != nil {
		t.Fatalf("fresh active snapshot: %d %s", freshRec.Code, freshRec.Body.String())
	}
	active = snapshot.Files[1]
	gitRec := authoringCall(t, s, "/git-commit", authoringGitCommitRequest{
		EditorPath: editorPath,
		Files: []authoringGitCommitFile{{
			Scope: active.Scope, Path: active.Path, ExpectedSHA256: active.SHA256,
		}},
		Message: "chore(test): reject active-only file",
	})
	if gitRec.Code != 400 || !strings.Contains(gitRec.Body.String(), "not manifest-declared") {
		t.Fatalf("active-only git commit: %d %s", gitRec.Code, gitRec.Body.String())
	}
}

func TestAuthoringBootstrapExposesManifestAndActiveFileThenUsesDeclaredScope(t *testing.T) {
	s, editorPath, manifest := authoringBootstrapFixture(t, bundle.ManifestFile)
	snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if snapshotRec.Code != 200 {
		t.Fatalf("snapshot: %d %s", snapshotRec.Code, snapshotRec.Body.String())
	}
	var snapshot authoringSnapshotResponse
	if err := json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 2 || snapshot.Files[0].Scope != bundle.AuthoringScopeBundle || snapshot.Files[0].Path != bundle.ManifestFile || snapshot.Files[1].Path != "main.bot" {
		t.Fatalf("bootstrap snapshot = %#v", snapshot)
	}
	if snapshot.ActiveFile == nil || snapshot.ActiveFile.Path != "main.bot" || snapshot.Files[0].GitCommittable || snapshot.Files[1].GitCommittable {
		t.Fatalf("bootstrap active file = %#v, files=%#v", snapshot.ActiveFile, snapshot.Files)
	}
	outside := authoringFileChange{
		Scope: "workspace", Path: "scripts/helper.py", ExpectedSHA256: "made-up",
		Replacements: []authoringReplacement{{Before: "return 41", After: "return 42"}},
	}
	if rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{outside}}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "neither the active editor file") {
		t.Fatalf("outside bootstrap scope: %d %s", rec.Code, rec.Body.String())
	}
	change := authoringFileChange{
		Scope: bundle.AuthoringScopeBundle, Path: bundle.ManifestFile, ExpectedSHA256: snapshot.Files[0].SHA256,
		Replacements: []authoringReplacement{{
			Before: manifest,
			After: `schema_version: 1
name: project-bot
authoring:
  editable_files:
    - {scope: bundle, path: manifest.yaml}
    - {scope: workspace, path: scripts/helper.py}
`,
		}},
	}
	if rec := authoringCall(t, s, "/commit", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}}); rec.Code != 200 {
		t.Fatalf("bootstrap commit: %d %s", rec.Code, rec.Body.String())
	}
	freshRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if freshRec.Code != 200 {
		t.Fatalf("fresh snapshot: %d %s", freshRec.Code, freshRec.Body.String())
	}
	var fresh authoringSnapshotResponse
	if err := json.Unmarshal(freshRec.Body.Bytes(), &fresh); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Files) != 3 || fresh.Files[1].Scope != bundle.AuthoringScopeWorkspace || fresh.Files[1].Path != "scripts/helper.py" || fresh.Files[2].Path != "main.bot" {
		t.Fatalf("declared snapshot = %#v", fresh)
	}
}

func TestAuthoringBootstrapRejectsStaleAmbiguousAndInvalidManifest(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		s, editorPath, manifest := authoringBootstrapFixture(t, bundle.ManifestFile)
		snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
		var snapshot authoringSnapshotResponse
		_ = json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot)
		path := filepath.Join(s.cfg.WorkDir, "project-bot", bundle.ManifestFile)
		if err := os.WriteFile(path, []byte(manifest+"description: newer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		change := authoringFileChange{Scope: bundle.AuthoringScopeBundle, Path: bundle.ManifestFile, ExpectedSHA256: snapshot.Files[0].SHA256, Replacements: []authoringReplacement{{Before: manifest, After: manifest + "description: changed\n"}}}
		if rec := authoringCall(t, s, "/commit", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}}); rec.Code != 409 {
			t.Fatalf("stale bootstrap: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		s, editorPath, _ := authoringBootstrapFixture(t, bundle.ManifestFile)
		snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
		var snapshot authoringSnapshotResponse
		_ = json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot)
		change := authoringFileChange{Scope: bundle.AuthoringScopeBundle, Path: bundle.ManifestFile, ExpectedSHA256: snapshot.Files[0].SHA256, Replacements: []authoringReplacement{{Before: " ", After: "  "}}}
		if rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "want exactly once") {
			t.Fatalf("ambiguous bootstrap: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("invalid manifest", func(t *testing.T) {
		s, editorPath, manifest := authoringBootstrapFixture(t, bundle.ManifestFile)
		snapshotRec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
		var snapshot authoringSnapshotResponse
		_ = json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot)
		change := authoringFileChange{Scope: bundle.AuthoringScopeBundle, Path: bundle.ManifestFile, ExpectedSHA256: snapshot.Files[0].SHA256, Replacements: []authoringReplacement{{Before: manifest, After: "schema_version: invalid\n"}}}
		if rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{change}}); rec.Code != 400 {
			t.Fatalf("invalid bootstrap manifest: %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAuthoringBootstrapUsesManifestYML(t *testing.T) {
	s, editorPath, _ := authoringBootstrapFixture(t, bundle.ManifestFileAlt)
	rec := authoringCall(t, s, "/snapshot", authoringSnapshotRequest{EditorPath: editorPath})
	if rec.Code != 200 {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	var snapshot authoringSnapshotResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &snapshot)
	if len(snapshot.Files) != 2 || snapshot.Files[0].Path != bundle.ManifestFileAlt || snapshot.Files[1].Path != "main.bot" {
		t.Fatalf("alternate manifest snapshot = %#v", snapshot)
	}
}

func TestAuthoringRejectsMaterializedDependencyAcrossEndpoints(t *testing.T) {
	root := t.TempDir()
	bundleDir := filepath.Join(root, ".botz", "shared-planner")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "main.bot"), []byte("workflow shared:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte(`schema_version: 1
name: shared-planner
authoring:
  editable_files:
    - {scope: bundle, path: main.bot}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: root}}
	paths := []string{".botz/shared-planner/main.bot"}
	if err := os.Symlink(bundleDir, filepath.Join(root, "shared-alias")); err == nil {
		paths = append(paths, "shared-alias/main.bot")
	}
	for _, editorPath := range paths {
		for _, endpoint := range []string{"/snapshot", "/preview", "/commit"} {
			var payload any = authoringSnapshotRequest{EditorPath: editorPath}
			if endpoint != "/snapshot" {
				payload = authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{{
					Scope: "bundle", Path: "main.bot", ExpectedSHA256: "irrelevant",
					Replacements: []authoringReplacement{{Before: "shared", After: "changed"}},
				}}}
			}
			rec := authoringCall(t, s, endpoint, payload)
			if rec.Code != 403 || !strings.Contains(rec.Body.String(), ".botz") {
				t.Fatalf("%s %s: %d %s", endpoint, editorPath, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestAuthoringRejectsWorkspaceCompanionInsideMaterializedDependency(t *testing.T) {
	root := t.TempDir()
	bundleDir := filepath.Join(root, "project-bot")
	lockedDir := filepath.Join(root, ".botz", "shared-planner")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lockedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "main.bot"), []byte("workflow main:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockedDir, "helper.py"), []byte("value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte(`schema_version: 1
name: project-bot
authoring:
  editable_files:
    - {scope: workspace, path: .botz/shared-planner/helper.py}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: root}}
	editorPath := "project-bot/main.bot"
	for _, endpoint := range []string{"/snapshot", "/preview", "/commit"} {
		var payload any = authoringSnapshotRequest{EditorPath: editorPath}
		if endpoint != "/snapshot" {
			payload = authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{{
				Scope: "workspace", Path: ".botz/shared-planner/helper.py", ExpectedSHA256: "irrelevant",
				Replacements: []authoringReplacement{{Before: "1", After: "2"}},
			}}}
		}
		rec := authoringCall(t, s, endpoint, payload)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), ".botz") {
			t.Fatalf("%s: %d %s", endpoint, rec.Code, rec.Body.String())
		}
	}
}

func TestAuthoringRejectsOutOfScopeBeforeReadAndStaleHash(t *testing.T) {
	s, editorPath := authoringFixture(t)
	outside := authoringFileChange{
		Scope: "workspace", Path: "secret.txt", ExpectedSHA256: "made-up",
		Replacements: []authoringReplacement{{Before: "secret", After: "stolen"}},
	}
	rec := authoringCall(t, s, "/preview", authoringChangeRequest{EditorPath: editorPath, Changes: []authoringFileChange{outside}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "neither the active editor file") {
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
