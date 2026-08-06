package automemory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/memory"
)

// fakeStore is an in-memory MemoryStore standing in for the CLOUD adapter:
// it has no filesystem behind it, which is exactly the case the mirror
// exists for. The FS adapter is exercised alongside it in every round-trip
// test, so a defect that only shows up on one backend cannot hide.
type fakeStore struct {
	mu            sync.Mutex
	docs          map[string][]byte
	writeErr      func(path string) error
	autoloadCalls int
	readCalls     int
}

var _ knowledge.MemoryStore = (*fakeStore)(nil)

func newFakeStore() *fakeStore { return &fakeStore{docs: map[string][]byte{}} }

func (m *fakeStore) Root(ref knowledge.SpaceRef) (string, error) { return "mem://" + ref.ID(), nil }

func (m *fakeStore) BuildIndex(context.Context, knowledge.SpaceRef) ([]knowledge.IndexEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	paths := make([]string, 0, len(m.docs))
	for p := range m.docs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]knowledge.IndexEntry, 0, len(paths))
	for _, p := range paths {
		// Both real adapters index Markdown only (pkg/memory/index.go,
		// pkg/store/mongo/memory.go). A double that returns everything makes
		// a defect look broader here than it is in production.
		if !strings.EqualFold(filepath.Ext(p), ".md") {
			continue
		}
		out = append(out, knowledge.IndexEntry{Path: p})
	}
	return out, nil
}

// Autoload mirrors the CLOUD adapter's semantics exactly (pkg/store/mongo:
// one bulk fetch, then filepath.Match per pattern) — including its blind
// spot: a path containing a glob metacharacter does not match itself. A stub
// that returned everything would certify a bulk path that silently drops
// documents in production.
func (m *fakeStore) Autoload(_ context.Context, _ knowledge.SpaceRef, patterns []string) ([]knowledge.AutoloadEntry, error) {
	m.autoloadCalls++
	if len(patterns) == 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []knowledge.AutoloadEntry
	for _, path := range slices.Sorted(maps.Keys(m.docs)) {
		for _, p := range patterns {
			if ok, _ := filepath.Match(p, path); ok {
				out = append(out, knowledge.AutoloadEntry{Path: path, Content: m.docs[path]})
				break
			}
		}
	}
	return out, nil
}

func (m *fakeStore) ListDocuments(context.Context, knowledge.SpaceRef, string) ([]knowledge.DocumentMeta, error) {
	return nil, nil
}

func (m *fakeStore) ReadDocument(_ context.Context, _ knowledge.SpaceRef, path string) (knowledge.Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readCalls++
	body, ok := m.docs[path]
	if !ok {
		return knowledge.Document{}, fmt.Errorf("%w: %s", knowledge.ErrDocNotFound, path)
	}
	return knowledge.Document{Meta: knowledge.DocumentMeta{Path: path, Size: int64(len(body))}, Content: body}, nil
}

func (m *fakeStore) WriteDocument(ctx context.Context, _ knowledge.SpaceRef, in knowledge.DocumentInput) (knowledge.DocumentMeta, error) {
	// Honour cancellation the way the CLOUD driver does. The filesystem
	// adapter ignores the context entirely, and a double that copied it hid a
	// real defect for a whole round: syncing on a cancelled run's context
	// discarded every note, visible only against Mongo.
	if err := ctx.Err(); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	if err := knowledge.ValidateDocPath(in.Path); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	if m.writeErr != nil {
		if err := m.writeErr(in.Path); err != nil {
			return knowledge.DocumentMeta{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[in.Path] = append([]byte(nil), in.Content...)
	return knowledge.DocumentMeta{Path: in.Path, Size: int64(len(in.Content))}, nil
}

func (m *fakeStore) DeleteDocument(ctx context.Context, _ knowledge.SpaceRef, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, path)
	return nil
}

func (m *fakeStore) UsageBytes(context.Context, knowledge.SpaceRef) (int64, int64, error) {
	return 0, 0, nil
}

// storeUnderTest pairs a store with a ref addressing a space inside it, so
// every round-trip test runs against BOTH the cloud-shaped fake and the real
// filesystem adapter.
type storeUnderTest struct {
	name  string
	store knowledge.MemoryStore
	ref   knowledge.SpaceRef
}

func bothStores(t *testing.T) []storeUnderTest {
	t.Helper()
	// ITERION_HOME roots the FS adapter under the test's own tree, so the
	// real on-disk layout is exercised without touching the operator's.
	t.Setenv("ITERION_HOME", t.TempDir())
	return []storeUnderTest{
		{"fake(cloud-shaped)", newFakeStore(), memory.LegacyBotRef(t.TempDir(), SpaceName)},
		{"fs", memory.DefaultFSStore(), memory.LegacyBotRef(t.TempDir(), SpaceName)},
	}
}

func mustHydrate(t *testing.T, m *Mirror) {
	t.Helper()
	if err := m.Hydrate(context.Background()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
}

// readSpace returns the space's documents as path → content.
func readSpace(t *testing.T, s knowledge.MemoryStore, ref knowledge.SpaceRef) map[string]string {
	t.Helper()
	index, err := s.BuildIndex(context.Background(), ref)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	out := map[string]string{}
	for _, e := range index {
		doc, err := s.ReadDocument(context.Background(), ref, e.Path)
		if err != nil {
			t.Fatalf("ReadDocument %q: %v", e.Path, err)
		}
		out[filepath.ToSlash(e.Path)] = string(doc.Content)
	}
	return out
}

func seed(t *testing.T, s knowledge.MemoryStore, ref knowledge.SpaceRef, docs map[string]string) {
	t.Helper()
	for path, body := range docs {
		if _, err := s.WriteDocument(context.Background(), ref, knowledge.DocumentInput{
			Path: path, Content: []byte(body),
		}); err != nil {
			t.Fatalf("seed %q: %v", path, err)
		}
	}
}

// The whole point of the mirror: what the agent leaves on disk becomes what
// the NEXT run reads out of the store — including on a store with no
// filesystem behind it, which is the cloud case.
func TestMirror_RoundTrip(t *testing.T) {
	for _, sut := range bothStores(t) {
		t.Run(sut.name, func(t *testing.T) {
			seed(t, sut.store, sut.ref, map[string]string{
				"MEMORY.md":         "- build: go test ./...",
				"topics/deploy.md":  "old deploy note",
				"topics/removed.md": "about to be deleted",
			})
			dir := t.TempDir()
			m := NewMirror(sut.store, sut.ref, dir, "bot:tester")
			mustHydrate(t, m)

			// Hydrate put everything on disk where the agent expects it.
			for _, rel := range []string{"MEMORY.md", "topics/deploy.md", "topics/removed.md"} {
				if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
					t.Fatalf("hydrate did not materialise %s: %v", rel, err)
				}
			}

			// The agent edits, adds and deletes.
			write(t, filepath.Join(dir, "MEMORY.md"), "- build: go test ./...\n- lint: task lint")
			write(t, filepath.Join(dir, "topics", "new.md"), "fresh note")
			if err := os.Remove(filepath.Join(dir, "topics", "removed.md")); err != nil {
				t.Fatal(err)
			}

			if err := m.SyncBack(context.Background()); err != nil {
				t.Fatalf("SyncBack: %v", err)
			}

			got := readSpace(t, sut.store, sut.ref)
			want := map[string]string{
				"MEMORY.md":        "- build: go test ./...\n- lint: task lint",
				"topics/deploy.md": "old deploy note",
				"topics/new.md":    "fresh note",
			}
			if len(got) != len(want) {
				t.Fatalf("space has %d docs, want %d: %v", len(got), len(want), got)
			}
			for path, body := range want {
				if got[path] != body {
					t.Errorf("%s = %q, want %q", path, got[path], body)
				}
			}
		})
	}
}

// A second run must SEE the first run's notes. This is the property the
// feature is for, and the one a cloud pod's ephemeral disk cannot provide.
func TestMirror_SurvivesAFreshDirectory(t *testing.T) {
	for _, sut := range bothStores(t) {
		t.Run(sut.name, func(t *testing.T) {
			first := NewMirror(sut.store, sut.ref, t.TempDir(), "bot:tester")
			mustHydrate(t, first)
			write(t, filepath.Join(first.Dir(), "MEMORY.md"), "- learned: the flag is --force")
			if err := first.SyncBack(context.Background()); err != nil {
				t.Fatalf("SyncBack: %v", err)
			}

			// A different directory entirely — a new worktree, a new pod.
			second := NewMirror(sut.store, sut.ref, t.TempDir(), "bot:tester")
			mustHydrate(t, second)
			body, err := os.ReadFile(filepath.Join(second.Dir(), "MEMORY.md"))
			if err != nil {
				t.Fatalf("second run cannot read the first run's memory: %v", err)
			}
			if !strings.Contains(string(body), "the flag is --force") {
				t.Errorf("memory did not survive: %q", body)
			}
		})
	}
}

// An untouched space must cost zero writes — otherwise every node of every
// run re-uploads the whole tree and burns the org's quota churn for nothing.
func TestMirror_UntouchedFilesAreNotRewritten(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{"MEMORY.md": "- unchanged"})

	m := NewMirror(store, ref, t.TempDir(), "bot:tester")
	mustHydrate(t, m)

	var written []string
	store.writeErr = func(path string) error {
		written = append(written, path)
		return nil
	}
	if err := m.SyncBack(context.Background()); err != nil {
		t.Fatalf("SyncBack: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("untouched space triggered writes: %v", written)
	}
}

// Memory is readable by every later run of the bot, so a token the agent
// happened to paste into a note must not be persisted there.
func TestMirror_RefusesSecretShapedContent(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	m := NewMirror(store, ref, t.TempDir(), "bot:tester")
	mustHydrate(t, m)

	write(t, filepath.Join(m.Dir(), "MEMORY.md"), "- the CI token is ghp_0123456789abcdefghijklmnopqrstuvwxyz")
	write(t, filepath.Join(m.Dir(), "safe.md"), "- nothing sensitive here")

	err := m.SyncBack(context.Background())
	if err == nil {
		t.Fatal("expected a reported refusal")
	}
	if !strings.Contains(err.Error(), "MEMORY.md") {
		t.Errorf("the refusal must name the offending file, got: %v", err)
	}
	got := readSpace(t, store, ref)
	if _, leaked := got["MEMORY.md"]; leaked {
		t.Error("secret-shaped content was persisted")
	}
	// One bad file must not cost the agent its other notes.
	if got["safe.md"] != "- nothing sensitive here" {
		t.Errorf("the clean document was dropped alongside it: %v", got)
	}
}

// The mirror can sit inside the target repository's checkout, so following a
// symlink would copy a host file of the repo's choosing into a store every
// later run of this bot reads.
func TestMirror_SkipsSymlinks(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	m := NewMirror(store, ref, t.TempDir(), "bot:tester")
	mustHydrate(t, m)

	secret := filepath.Join(t.TempDir(), "id_rsa.md")
	write(t, secret, "PRIVATE KEY MATERIAL")
	if err := os.Symlink(secret, filepath.Join(m.Dir(), "stolen.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	write(t, filepath.Join(m.Dir(), "MEMORY.md"), "- ordinary note")

	err := m.SyncBack(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the skipped symlink must be reported, got: %v", err)
	}
	got := readSpace(t, store, ref)
	if _, leaked := got["stolen.md"]; leaked {
		t.Fatal("content was read through a symlink into the store")
	}
	if got["MEMORY.md"] != "- ordinary note" {
		t.Errorf("the real note was dropped: %v", got)
	}
}

// Only Markdown is indexed by the store, so a non-.md file would be written
// and then be invisible to every reader. Say so rather than drop it silently.
func TestMirror_ReportsNonMarkdown(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	m := NewMirror(store, ref, t.TempDir(), "bot:tester")
	mustHydrate(t, m)

	write(t, filepath.Join(m.Dir(), "notes.txt"), "not markdown")
	err := m.SyncBack(context.Background())
	if err == nil || !strings.Contains(err.Error(), "only Markdown") {
		t.Errorf("expected a non-markdown report, got: %v", err)
	}
}

// A rejected write (over quota, in production) must not cost the agent the
// other documents it wrote in the same session.
func TestMirror_OneRejectedWriteKeepsTheRest(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	store.writeErr = func(path string) error {
		if path == "big.md" {
			return &knowledge.QuotaError{Used: 10, Delta: 5, Quota: 12}
		}
		return nil
	}
	m := NewMirror(store, ref, t.TempDir(), "bot:tester")
	mustHydrate(t, m)
	write(t, filepath.Join(m.Dir(), "big.md"), "too much")
	write(t, filepath.Join(m.Dir(), "MEMORY.md"), "- kept")

	err := m.SyncBack(context.Background())
	if err == nil || !errors.Is(err, knowledge.ErrQuotaExceeded) {
		t.Errorf("the quota failure must surface, got: %v", err)
	}
	if got := readSpace(t, store, ref); got["MEMORY.md"] != "- kept" {
		t.Errorf("the accepted document was lost: %v", got)
	}
}

func TestMirror_SyncBackBeforeHydrateIsAnError(t *testing.T) {
	m := NewMirror(newFakeStore(), memory.LegacyBotRef(t.TempDir(), SpaceName), t.TempDir(), "")
	if err := m.SyncBack(context.Background()); err == nil {
		t.Fatal("SyncBack without Hydrate must error rather than delete the space")
	}
}

// Hydrate creates the directory even for an empty space: the backend is
// pointed at it before the agent's first write, and claude_code's
// autoMemoryDirectory must resolve to something that exists.
func TestMirror_HydrateCreatesDirForEmptySpace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "auto-memory")
	m := NewMirror(newFakeStore(), memory.LegacyBotRef(t.TempDir(), SpaceName), dir, "")
	mustHydrate(t, m)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("Hydrate must create the mirror dir: %v", err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The mirror directory outlives a run — it sits under a state root shared by
// every run on the store. A document deleted from the space must therefore be
// removed from the directory at hydrate, or the agent keeps reading it and
// SyncBack uploads it straight back: the deletion would never stick.
func TestMirror_HydratePrunesDeletedDocuments(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{"MEMORY.md": "- kept", "gone.md": "- removed later"})

	dir := t.TempDir()
	first := NewMirror(store, ref, dir, "bot:tester")
	mustHydrate(t, first)

	// The space loses a document between runs (an operator edit, another run).
	if err := store.DeleteDocument(context.Background(), ref, "gone.md"); err != nil {
		t.Fatal(err)
	}

	second := NewMirror(store, ref, dir, "bot:tester")
	mustHydrate(t, second)
	if _, err := os.Stat(filepath.Join(dir, "gone.md")); err == nil {
		t.Error("a document deleted from the space is still in the mirror")
	}
	if err := second.SyncBack(context.Background()); err != nil {
		t.Fatalf("SyncBack: %v", err)
	}
	if got := readSpace(t, store, ref); len(got) != 1 || got["MEMORY.md"] != "- kept" {
		t.Errorf("the deletion was undone by the mirror: %v", got)
	}

	// Files the mirror does not manage are not its to delete.
	if err := os.WriteFile(filepath.Join(dir, "operator-notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustHydrate(t, NewMirror(store, ref, dir, "bot:tester"))
	if _, err := os.Stat(filepath.Join(dir, "operator-notes.txt")); err != nil {
		t.Errorf("pruning removed a file the mirror does not manage: %v", err)
	}
}

// Hydrate must use the store's BULK path. Per-document reads are one network
// round trip each on the cloud adapter, and this runs before every node of
// every run.
func TestMirror_HydrateReadsInBulk(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{
		"MEMORY.md": "a", "topics/one.md": "b", "topics/two.md": "c",
	})
	store.readCalls, store.autoloadCalls = 0, 0

	m := NewMirror(store, ref, t.TempDir(), "bot:tester")
	mustHydrate(t, m)

	if store.autoloadCalls != 1 {
		t.Errorf("Autoload calls = %d, want exactly 1 bulk fetch", store.autoloadCalls)
	}
	if store.readCalls != 0 {
		t.Errorf("per-document reads = %d, want 0 when the bulk path covers everything", store.readCalls)
	}
	for _, rel := range []string{"MEMORY.md", "topics/one.md", "topics/two.md"} {
		if _, err := os.Stat(filepath.Join(m.Dir(), rel)); err != nil {
			t.Errorf("bulk hydrate missed %s: %v", rel, err)
		}
	}
}

// The bulk path matches by glob, so a path carrying a metacharacter does not
// match itself. Such a document must still reach the agent — dropping it
// silently would hand over a truncated memory the agent then "corrects" by
// rewriting, destroying what it could not see.
func TestMirror_HydrateFallsBackForUnmatchablePaths(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{
		"MEMORY.md":   "plain",
		"notes[1].md": "bracketed",
	})
	store.readCalls, store.autoloadCalls = 0, 0

	m := NewMirror(store, ref, t.TempDir(), "bot:tester")
	mustHydrate(t, m)

	body, err := os.ReadFile(filepath.Join(m.Dir(), "notes[1].md"))
	if err != nil || string(body) != "bracketed" {
		t.Fatalf("a glob-unmatchable document was dropped: %q, %v", body, err)
	}
	if store.readCalls != 1 {
		t.Errorf("per-document fallback ran %d times, want exactly 1 (only the unmatched path)", store.readCalls)
	}
}

// Pruning walks NAMES, not bodies. A file too large to persist is exactly the
// case that separates the two: it is stat-ed and reported, never opened, and
// it is left alone — deleting an agent's oversized note would destroy work the
// operator can still recover by hand.
func TestMirror_PruneSkipsOversizedFilesWithoutReadingThem(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	dir := t.TempDir()

	write(t, filepath.Join(dir, "huge.md"), strings.Repeat("x", maxMirrorFileBytes+1))
	write(t, filepath.Join(dir, "stale.md"), "the space never had this")

	m := NewMirror(store, ref, dir, "bot:tester")
	mustHydrate(t, m)

	if _, err := os.Stat(filepath.Join(dir, "stale.md")); err == nil {
		t.Error("a persistable file the space does not have should have been pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "huge.md")); err != nil {
		t.Errorf("an oversized note must be left for the operator, not deleted: %v", err)
	}

	// And it is reported at sync-back rather than dropped in silence.
	err := m.SyncBack(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("the oversized note must be reported, got: %v", err)
	}
}

// The defer that removes a node's mirror does not run on SIGKILL or an OOM
// kill, and nothing else reaps them — without a sweep they accumulate for the
// life of the store.
func TestSweepStaleNodeDirs(t *testing.T) {
	root := t.TempDir()
	stale, releaseStale, err := NewNodeDir(root)
	if err != nil {
		t.Fatal(err)
	}
	// A crashed run leaves its lock behind with nothing holding it; releasing
	// here is what makes this directory look crashed rather than live.
	releaseStaleLockOnly(t, stale, releaseStale)
	fresh, releaseFresh, err := NewNodeDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFresh()
	// Something that is not ours must be left alone.
	keep := filepath.Join(root, "operator-notes")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-staleNodeDirAge - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keep, old, old); err != nil {
		t.Fatal(err)
	}

	SweepStaleNodeDirs(root, nil)

	if _, err := os.Stat(stale); err == nil {
		t.Errorf("a crashed run's mirror was not reaped: %s", stale)
	}
	// A young directory may belong to a run still in flight; taking it would
	// destroy the notes that run is about to sync.
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a live node's mirror was reaped: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the sweep took a directory it does not own: %v", err)
	}
}

// A mirror directory that vanishes mid-node must not read as "the agent
// deleted everything". Deletions are inferred from ABSENCE, so they are only
// safe when the walk actually looked — otherwise a concurrent run's sweep, a
// /tmp reaper or an operator wipes every document the bot ever recorded.
//
// Reproduced before the fix: three seeded documents, directory removed, space
// emptied, one warning.
// The mirror directory vanishes mid-node (a concurrent run's sweep, a /tmp
// reaper, an operator). Does SyncBack conclude "the agent deleted everything"?
func TestMirror_VanishedDirDoesNotDeleteTheSpace(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{
		"MEMORY.md": "years of notes", "topics/a.md": "a", "topics/b.md": "b",
	})
	dir := filepath.Join(t.TempDir(), "node-1")
	m := NewMirror(store, ref, dir, "bot:x")
	mustHydrate(t, m)

	if err := os.RemoveAll(dir); err != nil { // swept out from under the node
		t.Fatal(err)
	}
	err := m.SyncBack(context.Background())
	got := readSpace(t, store, ref)
	if err == nil {
		t.Error("a vanished mirror must be reported, not passed over in silence")
	}
	if len(got) == 0 {
		t.Fatalf("DESTRUCTIVE: the whole space was deleted because the mirror vanished")
	}
	if len(got) != 3 {
		t.Errorf("space lost documents: %v", got)
	}
}

// A live node's mirror must survive a concurrent run's sweep, however old it
// looks. Age alone cannot tell live from crashed: a directory's mtime does not
// move when a file inside it is overwritten, so a node that spends hours
// rewriting MEMORY.md looks abandoned. The lock is the signal — the OS drops
// it when the holder exits, which a crash does and a long node does not.
func TestSweepSparesALiveNodeHoweverOldItLooks(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{"MEMORY.md": "years of notes"})

	spaceRoot := t.TempDir()
	dir, release, err := NewNodeDir(spaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	live := NewMirror(store, ref, dir, "bot:x")
	mustHydrate(t, live)
	write(t, filepath.Join(dir, "MEMORY.md"), "years of notes\n- and one more")

	// Backdate past every age gate: only the lock stands between this node and
	// the sweep.
	old := time.Now().Add(-staleNodeDirAge - time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	SweepStaleNodeDirs(spaceRoot, nil) // a concurrent run starts

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a live node's mirror was reaped: %v", err)
	}
	if err := live.SyncBack(context.Background()); err != nil {
		t.Fatalf("SyncBack: %v", err)
	}
	if got := readSpace(t, store, ref); got["MEMORY.md"] != "years of notes\n- and one more" {
		t.Errorf("the live node's notes did not survive: %v", got)
	}
}

// A file the walk LISTED but could not open is not a file the agent deleted.
// Reading our own copy failing — a stray mode bit, a uid mismatch between host
// and sandbox, a network filesystem hiccup — used to drop the document from
// the store, with one warning. The round-3 fix covered the directory-level
// version of exactly this and left the file-level one open.
func TestMirror_UnreadableFileIsNotADeletion(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{"a.md": "A", "b.md": "B", "c.md": "C"})
	dir := t.TempDir()
	m := NewMirror(store, ref, dir, "bot:x")
	mustHydrate(t, m)

	if err := os.Chmod(filepath.Join(dir, "b.md"), 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "b.md"), 0o600) })

	err := m.SyncBack(context.Background())
	t.Logf("SyncBack err = %v", err)
	got := readSpace(t, store, ref)
	if _, ok := got["b.md"]; !ok {
		t.Fatalf("REGRESSION: an UNREADABLE file was deleted from the store; store = %v", got)
	}
}

// …and a genuine deletion must still propagate.
// …and the fix must not kill the feature it guards.
func TestMirror_RealDeletionStillPropagates(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{"keep.md": "K", "gone.md": "G"})
	dir := t.TempDir()
	m := NewMirror(store, ref, dir, "bot:x")
	mustHydrate(t, m)
	if err := os.Remove(filepath.Join(dir, "gone.md")); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncBack(context.Background()); err != nil {
		t.Fatalf("SyncBack: %v", err)
	}
	got := readSpace(t, store, ref)
	if _, still := got["gone.md"]; still {
		t.Errorf("a real deletion no longer propagates: %v", got)
	}
}

// releaseStaleLockOnly drops a node directory's lock while LEAVING the
// directory on disk — the state a SIGKILLed run leaves behind, which is
// exactly what the sweep exists to reap.
func releaseStaleLockOnly(t *testing.T, dir string, release func()) {
	t.Helper()
	release()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// A fixed budget silently truncates: chosen short enough to keep Cancel
// responsive, it loses whatever did not fit when the memory is large and the
// store slow — with one warning. The budget must scale with the work.
func TestSyncBudgetScalesWithTheWork(t *testing.T) {
	small, large := syncBudget(1), syncBudget(200)
	if small < 20*time.Second {
		t.Errorf("the ordinary case must not be tight: %v", small)
	}
	if large <= small {
		t.Errorf("a 200-document sync gets no more time than a 1-document one: %v vs %v", large, small)
	}
	// …and it stays bounded: past the cap the store is broken, not slow, and a
	// cancelled run must still return.
	if capped := syncBudget(100000); capped > 90*time.Second {
		t.Errorf("the budget is unbounded: %v", capped)
	}
}

// A caller that DID set a deadline keeps it — the mirror must not quietly
// extend someone else's bound.
func TestSyncBackHonoursACallerDeadline(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	dir, release, err := NewNodeDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	m := NewMirror(store, ref, dir, "bot:x")
	mustHydrate(t, m)
	write(t, filepath.Join(dir, "MEMORY.md"), "note")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.SyncBack(ctx); err == nil {
		t.Error("an already-cancelled caller context must not be overridden")
	}
}

// Whether a file can be STORED and whether the agent still HAS it are
// different questions. Answering the second with the first deleted the store's
// copy of a note the agent had merely grown past the size cap — an ordinary
// end for a long-lived memory, and the fifth variation on the same mistake.
// A long-lived memory grows past the per-document cap. The oversized copy is
// correctly not persisted — but does the ORIGINAL survive in the store?
func TestMirror_OversizedNoteDoesNotDeleteTheStoredCopy(t *testing.T) {
	store := newFakeStore()
	ref := memory.LegacyBotRef(t.TempDir(), SpaceName)
	seed(t, store, ref, map[string]string{"MEMORY.md": "years of accumulated notes"})

	dir, release, err := NewNodeDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	m := NewMirror(store, ref, dir, "bot:x")
	mustHydrate(t, m)

	// The agent keeps appending until the note passes the cap.
	write(t, filepath.Join(dir, "MEMORY.md"), strings.Repeat("x", maxMirrorFileBytes+1))

	err = m.SyncBack(context.Background())
	t.Logf("SyncBack err = %v", err)
	got := readSpace(t, store, ref)
	if _, ok := got["MEMORY.md"]; !ok {
		t.Fatalf("DATA LOSS: the store's copy was deleted because the disk copy grew too big; store = %v", got)
	}
	if got["MEMORY.md"] != "years of accumulated notes" {
		t.Errorf("the store's copy changed: %q", got["MEMORY.md"])
	}
	_ = os.Remove(filepath.Join(dir, "MEMORY.md"))
}

// The lock lives OUTSIDE the directory the agent is handed. An agent writes
// freely in its mirror, so a lock inside it is a lock the agent can overwrite —
// enough, on the Windows implementation, to make a live node look abandoned to
// the sweep.
func TestNodeLockIsOutOfTheAgentsReach(t *testing.T) {
	root := t.TempDir()
	dir, release, err := NewNodeDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the agent's directory must start empty, found %v", entries[0].Name())
	}

	// Whatever the agent does inside its own directory, the liveness signal
	// holds and the sweep spares it.
	write(t, filepath.Join(dir, ".lock"), "not the real one")
	write(t, filepath.Join(dir, "MEMORY.md"), "work in progress")
	old := time.Now().Add(-staleNodeDirAge - time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	SweepStaleNodeDirs(root, nil)
	if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err != nil {
		t.Errorf("the sweep took a live node after the agent wrote a decoy lock: %v", err)
	}
}

// release() is handed to a defer, which is exactly the shape a later refactor
// calls twice. Doing so raced on the lock's file handle.
func TestNodeDirReleaseIsIdempotent(t *testing.T) {
	dir, release, err := NewNodeDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if _, err := os.Stat(dir); err == nil {
		t.Error("the directory outlived a double release")
	}
	if _, err := os.Stat(nodeLockPath(dir)); err == nil {
		t.Error("the lock file was left behind")
	}
}

// TestSyncBackKeepsDocumentsStoredUnderANonCanonicalPath is the fourth
// reproduction of one root cause: inferring "the agent deleted this" from "I
// do not see it".
//
// Here the mirror could not even LOOK under the name the store uses. A
// document stored as "./MEMORY.md" (a spelling an older binary accepted, and
// which the cloud adapter keeps verbatim where the FS one normalises it) is
// materialised through the filesystem, which canonicalises — so the walk
// reports "MEMORY.md", the deletion loop asks whether "./MEMORY.md" came back,
// and destroys the note on both sides. The agent never ran.
//
// The path contract is where this is fixed (knowledge.ValidateDocPath now
// refuses a non-canonical spelling, so no writer can create one), and the
// mirror skips what it cannot materialise instead of failing the node — which
// is also what keeps such a row out of `hydrated`, and therefore out of reach
// of the deletion loop.
func TestSyncBackKeepsDocumentsStoredUnderANonCanonicalPath(t *testing.T) {
	for _, raw := range []string{"./MEMORY.md", "topics//deploy.md", "topics/./deploy.md"} {
		t.Run(raw, func(t *testing.T) {
			st := newFakeStore()
			st.docs[raw] = []byte("# a note worth keeping\n")

			m := NewMirror(st, knowledge.SpaceRef{}, filepath.Join(t.TempDir(), "mirror"), "bot:test")
			if err := m.Hydrate(context.Background()); err != nil {
				t.Fatalf("hydrate must not fail over one unusable row: %v", err)
			}
			// The agent does nothing at all — the strongest form of the case.
			err := m.SyncBack(context.Background())
			if err == nil {
				t.Errorf("the skipped document must be REPORTED, not dropped in silence")
			} else if !strings.Contains(err.Error(), raw) {
				t.Errorf("warning should name the skipped document, got: %v", err)
			}
			if _, ok := st.docs[raw]; !ok {
				t.Fatalf("DATA LOSS: %q was deleted although the agent never touched it (store: %v)", raw, st.docs)
			}
		})
	}
}
