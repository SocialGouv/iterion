package bundle

import (
	"archive/zip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func hashDir(t *testing.T, dir string) string {
	t.Helper()
	h, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func zipNames(t *testing.T, path string) []string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func hasDraftName(names []string) string {
	for _, n := range names {
		if strings.HasSuffix(strings.ToLower(n), ".bot.yaml") {
			return n
		}
	}
	return ""
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sortedKeys(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// buildZipBotz writes a `.botz` ZIP archive to dest by hand — the current
// container, produced without the packer so it can carry what the packer
// never writes.
func buildZipBotz(t *testing.T, dest string, files map[string]string) {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, n := range sortedKeys(files) {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// archiveBuilders are the two containers Open reads — the current ZIP and
// the legacy tar.gz — both built by hand, so an archive can carry a draft
// the way one packed before the rule, or by hand, does.
var archiveBuilders = map[string]func(t *testing.T, dest string, files map[string]string){
	"zip": buildZipBotz,
	"tar.gz": func(t *testing.T, dest string, files map[string]string) {
		t.Helper()
		var entries []tarEntry
		for _, n := range sortedKeys(files) {
			entries = append(entries, tarEntry{Name: n, Body: []byte(files[n])})
		}
		buildBotz(t, dest, entries)
	},
}

// An author document (`.bot.yaml`) is a draft of a .bot and never a member
// of the bundle: the archive leaves it out and says how many it left; the
// bundle's content hash does not see it — from either walker — so editing a
// draft changes no bundle's identity, the identity a dependant's lock
// compares.
func TestADraftNeverTravelsInAnArchive(t *testing.T) {
	bare := t.TempDir()
	writeTree(t, bare, map[string]string{"main.bot": minimalBotIter, "lib/x.bot": minimalBotIter})
	drafted := t.TempDir()
	writeTree(t, drafted, map[string]string{
		"main.bot": minimalBotIter, "lib/x.bot": minimalBotIter,
		"main.bot.yaml": "dsl: 2\n", "lib/X.BOT.YAML": "dsl: 2\n",
	})

	bareHash := hashDir(t, bare)
	if h := hashDir(t, drafted); h != bareHash {
		t.Fatalf("a draft changed the bundle's identity: %s vs %s", h, bareHash)
	}
	if h, err := collectContentHash(drafted); err != nil || h != bareHash {
		t.Fatalf("the extraction-side walker sees the draft the pack-side walker does not: %s vs %s (%v)", h, bareHash, err)
	}

	out := filepath.Join(t.TempDir(), "drafted.botz")
	res, err := PackDir(drafted, out)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if res.Drafts != 2 {
		t.Fatalf("the packer left %d draft(s) behind, want 2", res.Drafts)
	}
	if res.Hash != bareHash {
		t.Fatalf("the packed hash %s is not the bare tree's %s", res.Hash, bareHash)
	}
	if n := hasDraftName(zipNames(t, out)); n != "" {
		t.Fatalf("the archive carries a draft: %s", n)
	}
}

// A directory named like a draft is a directory: its files are members of
// the bundle — packed, hashed alike by both walkers, extracted, snapshotted
// — and never counted as drafts.
func TestADirectoryNamedLikeADraftIsADirectory(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"main.bot": minimalBotIter, "x.bot.yaml/inner.txt": "payload"})

	packSide := hashDir(t, dir)
	extractSide, err := collectContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if packSide != extractSide {
		t.Fatalf("the two hash walkers disagree on a tree with a directory named like a draft: %s vs %s", packSide, extractSide)
	}

	out := filepath.Join(t.TempDir(), "dir.botz")
	res, err := PackDir(dir, out)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if res.Drafts != 0 {
		t.Fatalf("a directory was counted as %d draft(s)", res.Drafts)
	}
	names := zipNames(t, out)
	found := false
	for _, n := range names {
		if n == "x.bot.yaml/inner.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the packer dropped the files of a directory named like a draft: %v", names)
	}

	b, cleanup, err := Open(out, t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cleanup()
	if b.Hash != packSide {
		t.Fatalf("the opened archive's hash %s is not the source tree's %s", b.Hash, packSide)
	}
	if !exists(filepath.Join(b.Dir, "x.bot.yaml", "inner.txt")) {
		t.Fatal("the extraction left out the files of a directory named like a draft")
	}

	snap := &Snapshot{Root: "parent"}
	if err := snap.AddDir("parent", dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Files["parent/x.bot.yaml/inner.txt"]; !ok {
		t.Fatalf("the snapshot left out the files of a directory named like a draft: %v", snap.Files)
	}
}

// Two archives that differ only by a draft hash alike, so they share the
// one content-addressed cache slot: whichever is opened first, in either
// container, the slot holds the same files and no draft — an archive packed
// before the rule, or by hand, is admitted with its draft left in the
// archive, never on disk. Proven in both orders: what lands in the slot is a
// fact of the archives, not of who came first.
func TestArchivesThatDifferByADraftShareOneSlotWithoutADraft(t *testing.T) {
	bot := minimalBotIter
	for container, build := range archiveBuilders {
		t.Run(container, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				draftedFirst bool
			}{{"drafted first", true}, {"plain first", false}} {
				t.Run(tc.name, func(t *testing.T) {
					plain := filepath.Join(t.TempDir(), "plain.botz")
					build(t, plain, map[string]string{"main.bot": bot})
					drafted := filepath.Join(t.TempDir(), "drafted.botz")
					build(t, drafted, map[string]string{"main.bot": bot, "main.bot.yaml": "dsl: 2\n"})
					first, second := plain, drafted
					if tc.draftedFirst {
						first, second = drafted, plain
					}
					cache := t.TempDir() // ONE cache root, as in production
					b1, c1, err := Open(first, cache)
					if err != nil {
						t.Fatalf("open first: %v", err)
					}
					defer c1()
					b2, c2, err := Open(second, cache)
					if err != nil {
						t.Fatalf("open second: %v", err)
					}
					defer c2()
					if b1.Hash != b2.Hash {
						t.Fatalf("the draft in the archive changed the identity: %s vs %s", b1.Hash, b2.Hash)
					}
					if b1.Dir != b2.Dir {
						t.Fatalf("one identity, two slots: %s vs %s", b1.Dir, b2.Dir)
					}
					if !exists(filepath.Join(b1.Dir, "main.bot")) {
						t.Fatal("the slot holds no main.bot")
					}
					if exists(filepath.Join(b1.Dir, "main.bot.yaml")) {
						t.Fatal("a draft landed in the slot: it travelled from the archive that carried it to every bundle that hashes alike")
					}
				})
			}
		})
	}
}

// The exported extraction — a bot install — leaves the draft in the archive
// too, in either container, and counts as written what it wrote.
func TestABotInstallExtractsNoDraft(t *testing.T) {
	for container, build := range archiveBuilders {
		t.Run(container, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "drafted.botz")
			build(t, archive, map[string]string{"main.bot": minimalBotIter, "main.bot.yaml": "dsl: 2\n"})
			f, err := os.Open(archive)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			dest := t.TempDir()
			n, err := ExtractArchive(f, dest)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if n != 1 {
				t.Fatalf("%d file(s) written, want 1 (the .bot)", n)
			}
			if !exists(filepath.Join(dest, "main.bot")) || exists(filepath.Join(dest, "main.bot.yaml")) {
				t.Fatal("the install extracted the draft, or not the .bot")
			}
		})
	}
}

// A snapshot freezes what a launch reads; a draft is not part of it and does
// not move its digest — and a snapshot persisted before the rule, carrying a
// draft, materialises without it: the runner reads the .bot alone.
func TestASnapshotLeavesTheDraftBehind(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"parent/main.bot": "parent-v1", "parent/skills/read.md": "skill"})
	bare := &Snapshot{Root: "parent"}
	if err := bare.AddDir("parent", filepath.Join(dir, "parent")); err != nil {
		t.Fatal(err)
	}
	_, bareDigest, err := bare.Encode()
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"parent/main.bot.yaml": "dsl: 2\n", "parent/lib/x.bot.yaml": "dsl: 2\n"})
	drafted := &Snapshot{Root: "parent"}
	if err := drafted.AddDir("parent", filepath.Join(dir, "parent")); err != nil {
		t.Fatal(err)
	}
	_, draftedDigest, err := drafted.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if bareDigest != draftedDigest {
		t.Fatalf("a draft moved the snapshot's digest: %s vs %s", bareDigest, draftedDigest)
	}
	for key := range drafted.Files {
		if strings.HasSuffix(key, ".bot.yaml") {
			t.Fatalf("the snapshot carries a draft: %s", key)
		}
	}

	old := &Snapshot{Root: "parent", Files: map[string]SnapshotFile{
		"parent/main.bot":      {Content: []byte("parent-v1")},
		"parent/main.bot.yaml": {Content: []byte("dsl: 2\n")},
	}}
	root, cleanup, err := old.Materialize()
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer cleanup()
	if !exists(filepath.Join(root, "main.bot")) {
		t.Fatal("the .bot was not materialised")
	}
	if exists(filepath.Join(root, "main.bot.yaml")) {
		t.Fatal("a draft a snapshot carried from before the rule was materialised on the runner")
	}
}

// A symlink named like a draft is a symlink: refused as any other is, never
// counted as a draft.
func TestASymlinkNamedLikeADraftIsRefusedLikeAnySymlink(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"main.bot": minimalBotIter})
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "evil.bot.yaml")); err != nil {
		t.Skipf("symlink not supported on this platform: %v", err)
	}
	_, err := PackDir(dir, filepath.Join(t.TempDir(), "o.botz"))
	errContains(t, err, "symlink")
}

// A plugin's source tree is not a bundle: PackTree keeps an author document
// like any other file, and counts no draft.
func TestAPluginTreeKeepsAnAuthorDocument(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"plugin.yaml": "name: p\n", "bots/x/main.bot.yaml": "dsl: 2\n"})
	out := filepath.Join(t.TempDir(), "plugin.zip")
	res, err := PackTree(dir, out)
	if err != nil {
		t.Fatalf("pack tree: %v", err)
	}
	if res.Drafts != 0 {
		t.Fatalf("a plugin tree counted %d draft(s)", res.Drafts)
	}
	if n := hasDraftName(zipNames(t, out)); n == "" {
		t.Fatalf("PackTree dropped a plugin's file because of its name: %v", zipNames(t, out))
	}
}
