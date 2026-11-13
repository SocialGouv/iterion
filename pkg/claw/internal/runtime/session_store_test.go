package runtime

import (
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// persistSessionViaStore is a test helper that creates and saves a session
// through a SessionStore.
func persistSessionViaStore(store *SessionStore, text string) *Session {
	session := NewSession()
	session.Messages = append(session.Messages, api.Message{
		Role:    "user",
		Content: []api.ContentBlock{{Type: "text", Text: text}},
	})
	_ = SaveSessionJSONL(store.sessionsRoot, session)
	return session
}

// persistSessionForFreeFunc is a test helper that creates and saves a session
// using the free function API.
func persistSessionForFreeFunc(root string, text string) *Session {
	session := NewSession()
	session.Messages = append(session.Messages, api.Message{
		Role:    "user",
		Content: []api.ContentBlock{{Type: "text", Text: text}},
	})
	handle, _ := CreateManagedSessionHandleFor(root, session.ID)
	dir := filepath.Dir(handle.Path)
	_ = SaveSessionJSONL(dir, session)
	return session
}

func TestCreatesAndListsManagedSessions(t *testing.T) {
	root := t.TempDir()
	older := persistSessionForFreeFunc(root, "older session")
	waitForNextMillisecond()
	newer := persistSessionForFreeFunc(root, "newer session")

	sessions, err := ListManagedSessionsFor(root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0].ID != newer.ID {
		t.Errorf("expected newest first, got %s", sessions[0].ID)
	}

	olderSummary := summaryByID(t, sessions, older.ID)
	newerSummary := summaryByID(t, sessions, newer.ID)
	if olderSummary.MessageCount != 1 {
		t.Errorf("older message count = %d, want 1", olderSummary.MessageCount)
	}
	if newerSummary.MessageCount != 1 {
		t.Errorf("newer message count = %d, want 1", newerSummary.MessageCount)
	}
}

func TestResolvesLatestAliasAndLoadsSession(t *testing.T) {
	root := t.TempDir()
	older := persistSessionForFreeFunc(root, "older session")
	waitForNextMillisecond()
	newer := persistSessionForFreeFunc(root, "newer session")

	handle, err := ResolveSessionReferenceFor(root, LatestSessionReference)
	if err != nil {
		t.Fatalf("resolve latest: %v", err)
	}

	loaded, err := LoadManagedSessionFor(root, "recent")
	if err != nil {
		t.Fatalf("load recent: %v", err)
	}

	if handle.ID != newer.ID {
		t.Errorf("latest handle ID = %s, want %s", handle.ID, newer.ID)
	}
	if loaded.Handle.ID != newer.ID {
		t.Errorf("loaded handle ID = %s, want %s", loaded.Handle.ID, newer.ID)
	}
	if len(loaded.Session.Messages) != 1 {
		t.Errorf("loaded messages = %d, want 1", len(loaded.Session.Messages))
	}
	if loaded.Handle.ID == older.ID {
		t.Error("loaded should not be the older session")
	}
	if !IsSessionReferenceAlias("last") {
		t.Error("expected 'last' to be an alias")
	}
}

func TestForksSessionWithLineage(t *testing.T) {
	root := t.TempDir()
	source := persistSessionForFreeFunc(root, "parent session")

	forked, err := ForkManagedSessionFor(root, source, "incident-review")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	sessions, err := ListManagedSessionsFor(root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	summary := summaryByID(t, sessions, forked.Handle.ID)

	if forked.ParentSessionID != source.ID {
		t.Errorf("parent = %s, want %s", forked.ParentSessionID, source.ID)
	}
	if forked.BranchName != "incident-review" {
		t.Errorf("branch = %s, want incident-review", forked.BranchName)
	}
	if summary.ParentSessionID != source.ID {
		t.Errorf("summary parent = %s, want %s", summary.ParentSessionID, source.ID)
	}
	if summary.BranchName != "incident-review" {
		t.Errorf("summary branch = %s, want incident-review", summary.BranchName)
	}
}

func TestWorkspaceFingerprintDeterministicAndDiffers(t *testing.T) {
	pathA := "/tmp/worktree-alpha"
	pathB := "/tmp/worktree-beta"

	fpA1 := WorkspaceFingerprint(pathA)
	fpA2 := WorkspaceFingerprint(pathA)
	fpB := WorkspaceFingerprint(pathB)

	if fpA1 != fpA2 {
		t.Errorf("same path must produce same fingerprint: %s vs %s", fpA1, fpA2)
	}
	if fpA1 == fpB {
		t.Errorf("different paths must produce different fingerprints: %s vs %s", fpA1, fpB)
	}
	if len(fpA1) != 16 {
		t.Errorf("fingerprint length = %d, want 16", len(fpA1))
	}

	// Cross-language parity: pin the expected hex from Go's hash/fnv.New64a()
	// which uses identical FNV-1a constants as Rust.
	expectedA := "ae5cd9e2efd7ae18"
	if fpA1 != expectedA {
		t.Errorf("cross-language parity: got %s, want %s for /tmp/worktree-alpha", fpA1, expectedA)
	}
}

func TestSessionStoreFromCWDIsolates(t *testing.T) {
	base := t.TempDir()
	workspaceA := filepath.Join(base, "repo-alpha")
	workspaceB := filepath.Join(base, "repo-beta")
	os.MkdirAll(workspaceA, 0o755)
	os.MkdirAll(workspaceB, 0o755)

	storeA, err := NewSessionStoreFromCWD(workspaceA)
	if err != nil {
		t.Fatalf("store a: %v", err)
	}
	storeB, err := NewSessionStoreFromCWD(workspaceB)
	if err != nil {
		t.Fatalf("store b: %v", err)
	}

	sessionA := persistSessionViaStore(storeA, "alpha work")
	_ = persistSessionViaStore(storeB, "beta work")

	listA, err := storeA.ListSessions()
	if err != nil {
		t.Fatalf("list a: %v", err)
	}
	listB, err := storeB.ListSessions()
	if err != nil {
		t.Fatalf("list b: %v", err)
	}

	if len(listA) != 1 {
		t.Errorf("store a sessions = %d, want 1", len(listA))
	}
	if len(listB) != 1 {
		t.Errorf("store b sessions = %d, want 1", len(listB))
	}
	if listA[0].ID != sessionA.ID {
		t.Errorf("store a session ID = %s, want %s", listA[0].ID, sessionA.ID)
	}
	if storeA.SessionsDir() == storeB.SessionsDir() {
		t.Error("session directories must differ across workspaces")
	}
}

func TestSessionStoreFromDataDirNamespaces(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "global-data")
	workspaceA := "/tmp/project-one"
	workspaceB := "/tmp/project-two"
	os.MkdirAll(dataDir, 0o755)

	storeA, err := NewSessionStoreFromDataDir(dataDir, workspaceA)
	if err != nil {
		t.Fatalf("store a: %v", err)
	}
	storeB, err := NewSessionStoreFromDataDir(dataDir, workspaceB)
	if err != nil {
		t.Fatalf("store b: %v", err)
	}

	persistSessionViaStore(storeA, "work in project-one")
	persistSessionViaStore(storeB, "work in project-two")

	if storeA.SessionsDir() == storeB.SessionsDir() {
		t.Error("data-dir stores must namespace by workspace")
	}

	listA, _ := storeA.ListSessions()
	listB, _ := storeB.ListSessions()
	if len(listA) != 1 {
		t.Errorf("store a sessions = %d, want 1", len(listA))
	}
	if len(listB) != 1 {
		t.Errorf("store b sessions = %d, want 1", len(listB))
	}
	if storeA.WorkspaceRoot() != workspaceA {
		t.Errorf("workspace root a = %s, want %s", storeA.WorkspaceRoot(), workspaceA)
	}
	if storeB.WorkspaceRoot() != workspaceB {
		t.Errorf("workspace root b = %s, want %s", storeB.WorkspaceRoot(), workspaceB)
	}
}

func TestSessionStoreCreateAndLoadRoundTrip(t *testing.T) {
	base := t.TempDir()
	store, err := NewSessionStoreFromCWD(base)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	session := persistSessionViaStore(store, "round-trip message")

	loaded, err := store.LoadSession(session.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.Handle.ID != session.ID {
		t.Errorf("loaded ID = %s, want %s", loaded.Handle.ID, session.ID)
	}
	if len(loaded.Session.Messages) != 1 {
		t.Errorf("loaded messages = %d, want 1", len(loaded.Session.Messages))
	}
}

func TestSessionStoreLatestAndResolveReference(t *testing.T) {
	base := t.TempDir()
	store, err := NewSessionStoreFromCWD(base)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = persistSessionViaStore(store, "older")
	waitForNextMillisecond()
	newer := persistSessionViaStore(store, "newer")

	latest, err := store.LatestSession()
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	handle, err := store.ResolveReference("latest")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if latest.ID != newer.ID {
		t.Errorf("latest ID = %s, want %s", latest.ID, newer.ID)
	}
	if handle.ID != newer.ID {
		t.Errorf("handle ID = %s, want %s", handle.ID, newer.ID)
	}
}

func TestSessionStoreForkStaysInNamespace(t *testing.T) {
	base := t.TempDir()
	store, err := NewSessionStoreFromCWD(base)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	source := persistSessionViaStore(store, "parent work")

	forked, err := store.ForkSession(source, "bugfix")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	sessions, err := store.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(sessions))
	}
	if forked.ParentSessionID != source.ID {
		t.Errorf("parent = %s, want %s", forked.ParentSessionID, source.ID)
	}
	if forked.BranchName != "bugfix" {
		t.Errorf("branch = %s, want bugfix", forked.BranchName)
	}

	// Forked session path must be inside the store namespace.
	storeDir := store.SessionsDir()
	if !isSubpath(forked.Handle.Path, storeDir) {
		t.Errorf("forked path %s not inside store dir %s", forked.Handle.Path, storeDir)
	}
}

// --- Helpers ---

func summaryByID(t *testing.T, summaries []ManagedSessionSummary, id string) ManagedSessionSummary {
	t.Helper()
	for _, s := range summaries {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session summary not found for ID %s", id)
	return ManagedSessionSummary{}
}

func isSubpath(path, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	if err != nil {
		return false
	}
	return !filepath.IsAbs(rel) && !strings.HasPrefix(rel, "..")
}

// waitForNextMillisecond busy-waits until the millisecond counter increments.
// Used to ensure distinct modification timestamps between test sessions.
func waitForNextMillisecond() {
	start := time.Now().UnixMilli()
	for time.Now().UnixMilli() <= start {
		// spin
	}
}
