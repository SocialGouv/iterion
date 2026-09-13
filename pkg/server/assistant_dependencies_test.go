package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestAssistantDependencyBotsUpdateCommitsOnlyLock(t *testing.T) {
	s, targetRef := assistantDependencyFixture(t)
	// Deliberately dirty but unstaged user work: it must survive the action.
	unrelated := filepath.Join(s.cfg.WorkDir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("operator work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := assistantDependencyCall(t, s, assistantDependencyBotsUpdateRequest{
		Name: "shared-planner", Ref: targetRef, Message: "chore(bots): pin shared planner",
	})
	if rec.Code != 200 {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	var got assistantDependencyBotsUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "shared-planner" || got.Ref != targetRef || got.Commit == "" || got.PreviousRef == targetRef {
		t.Fatalf("response = %#v", got)
	}
	lock, err := botlock.Load(s.cfg.WorkDir)
	if err != nil || lock.Dependencies[got.Name].Ref != targetRef || lock.Dependencies[got.Name].BundleSHA256 != got.BundleSHA256 {
		t.Fatalf("lock = %#v, err=%v", lock, err)
	}
	changed, err := runAuthoringGit(t.Context(), s.cfg.WorkDir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil || !sameGitPathSet(splitGitLines(changed), []string{botlock.FileName}) {
		t.Fatalf("committed paths = %q, err=%v", changed, err)
	}
	if body, err := os.ReadFile(unrelated); err != nil || string(body) != "operator work\n" {
		t.Fatalf("unrelated work = %q, err=%v", body, err)
	}
}

func TestAssistantDependencyBotsUpdateRejectsDirtyLockAndStagedIndex(t *testing.T) {
	t.Run("changed lock", func(t *testing.T) {
		s, targetRef := assistantDependencyFixture(t)
		lockPath := filepath.Join(s.cfg.WorkDir, botlock.FileName)
		if err := os.WriteFile(lockPath, []byte("version: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if rec := assistantDependencyCall(t, s, assistantDependencyBotsUpdateRequest{Name: "shared-planner", Ref: targetRef, Message: "chore(bots): pin shared planner"}); rec.Code != 409 {
			t.Fatalf("changed lock = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unrelated staged file", func(t *testing.T) {
		s, targetRef := assistantDependencyFixture(t)
		staged := filepath.Join(s.cfg.WorkDir, "staged.txt")
		if err := os.WriteFile(staged, []byte("do not absorb\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runAuthoringGit(t.Context(), s.cfg.WorkDir, "add", "--", "staged.txt")
		if rec := assistantDependencyCall(t, s, assistantDependencyBotsUpdateRequest{Name: "shared-planner", Ref: targetRef, Message: "chore(bots): pin shared planner"}); rec.Code != 409 {
			t.Fatalf("staged index = %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestValidateAssistantDependencyBotsUpdate(t *testing.T) {
	valid := assistantDependencyBotsUpdateRequest{Name: "shared-planner", Ref: strings.Repeat("a", 40), Message: "chore(bots): pin shared planner"}
	if err := validateAssistantDependencyBotsUpdate(valid); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	for _, req := range []assistantDependencyBotsUpdateRequest{
		{Name: "../planner", Ref: valid.Ref, Message: valid.Message},
		{Name: valid.Name, Ref: strings.Repeat("A", 40), Message: valid.Message},
		{Name: valid.Name, Ref: valid.Ref[:39], Message: valid.Message},
		{Name: valid.Name, Ref: valid.Ref, Message: "two\nlines"},
	} {
		if err := validateAssistantDependencyBotsUpdate(req); err == nil {
			t.Fatalf("invalid request accepted: %#v", req)
		}
	}
}

func TestAssistantDependencyBotsLocalizeMakesBoundedLocalSourceAndLockCommits(t *testing.T) {
	s, root, installedHash := assistantDependencyLocalizeFixture(t)
	cachePath := filepath.Join(root, ".botz", "shared-planner", "scripts", "__pycache__", "tool.cpython-314.pyc")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("runtime cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("operator work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{
		EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source",
	})
	if rec.Code != 200 {
		t.Fatalf("localize = %d %s", rec.Code, rec.Body.String())
	}
	var got assistantDependencyBotsLocalizeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "shared-planner" || got.LocalSource != "bots/planner/plugins/shared-planner" || got.SourceCommit == "" || got.LockCommit == "" || got.SourceCommit == got.LockCommit || got.BundleSHA256 != installedHash || got.ResumedAfterImport {
		t.Fatalf("response = %#v", got)
	}
	localDir := filepath.Join(root, filepath.FromSlash(got.LocalSource))
	if hash, err := bundle.ContentHashDir(localDir); err != nil || hash != installedHash {
		t.Fatalf("local source hash = %q, err=%v", hash, err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "scripts", "__pycache__", "tool.cpython-314.pyc")); !os.IsNotExist(err) {
		t.Fatalf("local source must not copy ignored runtime cache: %v", err)
	}
	if err := requireCommitDependencyPaths(t.Context(), root, got.SourceCommit, []string{
		"bots/planner/plugins/shared-planner/main.bot",
		"bots/planner/plugins/shared-planner/manifest.yaml",
	}); err != nil {
		t.Fatal(err)
	}
	if err := requireCommitDependencyPaths(t.Context(), root, got.LockCommit, []string{botlock.FileName}); err != nil {
		t.Fatal(err)
	}
	lock, err := botlock.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	dep := lock.Dependencies["shared-planner"]
	if dep.Source != "bots/planner/plugins" || dep.Path != "shared-planner" || dep.Ref != got.SourceCommit || dep.BundleSHA256 != installedHash {
		t.Fatalf("localized lock = %#v", dep)
	}
	if hash, err := bundle.ContentHashDir(filepath.Join(root, ".botz", "shared-planner")); err != nil || hash != installedHash {
		t.Fatalf("materialized hash = %q, err=%v", hash, err)
	}
	if body, err := os.ReadFile(unrelated); err != nil || string(body) != "operator work\n" {
		t.Fatalf("unrelated work = %q, err=%v", body, err)
	}
	status, err := assistantDependencyGit(t.Context(), root, "status", "--porcelain=v1", "--", ".botz")
	if err != nil || strings.TrimSpace(status) != "" {
		t.Fatalf("materialized cache must stay ignored/untracked: %q, err=%v", status, err)
	}

	// A later Copi source commit must be the pin used by the existing narrow
	// refresh action, not the earlier import commit or its following lock commit.
	localMain := filepath.Join(localDir, "main.bot")
	if err := os.WriteFile(localMain, []byte("workflow refreshed {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := assistantDependencyGit(t.Context(), root, "add", "--", filepath.ToSlash(filepath.Join(got.LocalSource, "main.bot"))); err != nil {
		t.Fatal(err)
	}
	if _, err := assistantDependencyGit(t.Context(), root, "commit", "-m", "fix(planner): refresh local hierarchy source"); err != nil {
		t.Fatal(err)
	}
	correctionCommit, err := assistantDependencyGit(t.Context(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	correctionCommit = strings.TrimSpace(correctionCommit)
	update := assistantDependencyCall(t, s, assistantDependencyBotsUpdateRequest{
		Name: "shared-planner", Ref: correctionCommit, Message: "chore(bots): refresh local shared planner",
	})
	if update.Code != 200 {
		t.Fatalf("refresh localized source = %d %s", update.Code, update.Body.String())
	}
	lock, err = botlock.Load(root)
	if err != nil || lock.Dependencies["shared-planner"].Ref != correctionCommit {
		t.Fatalf("refresh must pin correction commit: lock=%#v err=%v", lock, err)
	}
}

func TestAssistantDependencyBotsLocalizeRefusesUnsafeOrAlreadyLocalizedState(t *testing.T) {
	t.Run("staged work", func(t *testing.T) {
		s, root, _ := assistantDependencyLocalizeFixture(t)
		staged := filepath.Join(root, "staged.txt")
		if err := os.WriteFile(staged, []byte("do not absorb\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runAuthoringGit(t.Context(), root, "add", "--", "staged.txt")
		if rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source"}); rec.Code != 409 {
			t.Fatalf("staged localization = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unknown or mismatched dependency", func(t *testing.T) {
		s, root, _ := assistantDependencyLocalizeFixture(t)
		if rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "unknown", Message: "chore(planner): vendor source"}); rec.Code != 409 {
			t.Fatalf("unknown dependency = %d %s", rec.Code, rec.Body.String())
		}
		if err := os.WriteFile(filepath.Join(root, ".botz", "shared-planner", "main.bot"), []byte("workflow tampered {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor source"}); rec.Code != 409 {
			t.Fatalf("mismatched materialization = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("pre-existing destination", func(t *testing.T) {
		s, root, _ := assistantDependencyLocalizeFixture(t)
		path := filepath.Join(root, "bots", "planner", "plugins", "shared-planner")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "marker"), []byte("operator source\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor source"}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "already exists") {
			t.Fatalf("pre-existing destination = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("already local", func(t *testing.T) {
		s, _, _ := assistantDependencyLocalizeFixture(t)
		req := assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source"}
		if rec := assistantDependencyLocalizeCall(t, s, req); rec.Code != 200 {
			t.Fatalf("first localize = %d %s", rec.Code, rec.Body.String())
		}
		if rec := assistantDependencyLocalizeCall(t, s, req); rec.Code != 409 || !strings.Contains(rec.Body.String(), "already localized") {
			t.Fatalf("second localize = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("resume after source-only import", func(t *testing.T) {
		s, root, hash := assistantDependencyLocalizeFixture(t)
		localDir := filepath.Join(root, "bots", "planner", "plugins", "shared-planner")
		if err := copyDependencyLocalizationTree(filepath.Join(root, ".botz", "shared-planner"), localDir); err != nil {
			t.Fatal(err)
		}
		if _, err := assistantDependencyGit(t.Context(), root, "add", "--", "bots/planner/plugins/shared-planner"); err != nil {
			t.Fatal(err)
		}
		if _, err := assistantDependencyGit(t.Context(), root, "commit", "-m", "chore(planner): vendor shared planner source"); err != nil {
			t.Fatal(err)
		}
		rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source"})
		if rec.Code != 200 {
			t.Fatalf("resumed localization = %d %s", rec.Code, rec.Body.String())
		}
		var got assistantDependencyBotsLocalizeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.ResumedAfterImport || got.BundleSHA256 != hash {
			t.Fatalf("resumed response = %#v, err=%v", got, err)
		}
	})
	t.Run("resume after exact staged source import", func(t *testing.T) {
		s, root, hash := assistantDependencyLocalizeFixture(t)
		localDir := filepath.Join(root, "bots", "planner", "plugins", "shared-planner")
		if err := copyDependencyLocalizationTree(filepath.Join(root, ".botz", "shared-planner"), localDir); err != nil {
			t.Fatal(err)
		}
		if _, err := assistantDependencyGit(t.Context(), root, "add", "--", "bots/planner/plugins/shared-planner"); err != nil {
			t.Fatal(err)
		}
		rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source"})
		if rec.Code != 200 {
			t.Fatalf("resumed staged localization = %d %s", rec.Code, rec.Body.String())
		}
		var got assistantDependencyBotsLocalizeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.ResumedAfterImport || got.BundleSHA256 != hash {
			t.Fatalf("resumed staged response = %#v, err=%v", got, err)
		}
		if err := requireCommitDependencyPaths(t.Context(), root, got.SourceCommit, []string{
			"bots/planner/plugins/shared-planner/main.bot",
			"bots/planner/plugins/shared-planner/manifest.yaml",
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rejects staged import with an extra path", func(t *testing.T) {
		s, root, _ := assistantDependencyLocalizeFixture(t)
		localDir := filepath.Join(root, "bots", "planner", "plugins", "shared-planner")
		if err := copyDependencyLocalizationTree(filepath.Join(root, ".botz", "shared-planner"), localDir); err != nil {
			t.Fatal(err)
		}
		if _, err := assistantDependencyGit(t.Context(), root, "add", "--", "bots/planner/plugins/shared-planner"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "extra-staged.txt"), []byte("operator work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := assistantDependencyGit(t.Context(), root, "add", "--", "extra-staged.txt"); err != nil {
			t.Fatal(err)
		}
		head, err := assistantDependencyGit(t.Context(), root, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source"})
		if rec.Code != 409 {
			t.Fatalf("extra staged localization = %d %s", rec.Code, rec.Body.String())
		}
		after, err := assistantDependencyGit(t.Context(), root, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(after) != strings.TrimSpace(head) {
			t.Fatalf("rejected staged import must not commit: before=%q after=%q err=%v", head, after, err)
		}
		staged, err := assistantDependencyGit(t.Context(), root, "diff", "--cached", "--name-only")
		if err != nil || !strings.Contains(staged, "extra-staged.txt") {
			t.Fatalf("rejected staged import must preserve operator staging: %q err=%v", staged, err)
		}
	})
}

func assistantDependencyFixture(t *testing.T) (*Server, string) {
	t.Helper()
	sourceRoot := t.TempDir()
	bundleDir := filepath.Join(sourceRoot, "bots", "shared-planner")
	writeAssistantDependencyBundle(t, bundleDir, "0.1.0", "workflow shared {}\n")
	initAuthoringGit(t, sourceRoot)
	oldRef, err := runAuthoringGit(t.Context(), sourceRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	oldRef = strings.TrimSpace(oldRef)
	oldHash, err := bundle.ContentHashDir(bundleDir)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	lock := fmt.Sprintf("version: 1\ndependencies:\n  shared-planner:\n    source: %s\n    ref: %s\n    path: bots/shared-planner\n    bundle_sha256: %s\n", sourceRoot, oldRef, oldHash)
	if err := os.WriteFile(filepath.Join(root, botlock.FileName), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initAuthoringGit(t, root)

	writeAssistantDependencyBundle(t, bundleDir, "0.2.0", "workflow refreshed {}\n")
	if _, err := runAuthoringGit(t.Context(), sourceRoot, "add", "--", "bots/shared-planner"); err != nil {
		t.Fatal(err)
	}
	if _, err := runAuthoringGit(t.Context(), sourceRoot, "commit", "-qm", "refresh shared planner"); err != nil {
		t.Fatal(err)
	}
	targetRef, err := runAuthoringGit(t.Context(), sourceRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: Config{WorkDir: root}}, strings.TrimSpace(targetRef)
}

func writeAssistantDependencyBundle(t *testing.T, dir, version, workflow string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(fmt.Sprintf("name: shared-planner\nversion: %s\nschema_version: 1\nenabled: false\n", version)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assistantDependencyCall(t *testing.T, s *Server, payload assistantDependencyBotsUpdateRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/assistant/dependencies/bots-update", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAssistantDependencyBotsUpdate(rec, req)
	return rec
}

func assistantDependencyLocalizeFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	sourceRoot := t.TempDir()
	sourceBundle := filepath.Join(sourceRoot, "shared-planner")
	writeAssistantDependencyBundle(t, sourceBundle, "0.1.0", "workflow shared {}\n")
	initAuthoringGit(t, sourceRoot)
	ref, err := runAuthoringGit(t.Context(), sourceRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bundle.ContentHashDir(sourceBundle)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	plannerDir := filepath.Join(root, "bots", "planner")
	if err := os.MkdirAll(plannerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plannerDir, "main.bot"), []byte("workflow planner {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: planner\nschema_version: 1\ndependencies:\n  workflows:\n    - name: shared-planner\nauthoring:\n  editable_files:\n    - {scope: bundle, path: main.bot}\n"
	if err := os.WriteFile(filepath.Join(plannerDir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("version: 1\ndependencies:\n  shared-planner:\n    source: %s\n    ref: %s\n    path: shared-planner\n    bundle_sha256: %s\n", sourceRoot, strings.TrimSpace(ref), hash)
	if err := os.WriteFile(filepath.Join(root, botlock.FileName), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".botz/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initAuthoringGit(t, root)
	if err := copyDependencyLocalizationTree(sourceBundle, filepath.Join(root, ".botz", "shared-planner")); err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: Config{WorkDir: root}}, root, hash
}

func assistantDependencyLocalizeCall(t *testing.T, s *Server, payload assistantDependencyBotsLocalizeRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/assistant/dependencies/bots-localize", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAssistantDependencyBotsLocalize(rec, req)
	return rec
}
