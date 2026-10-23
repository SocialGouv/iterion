package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// putManifest sends the studio's metadata edit of bot meta-bot.
func putManifest(s *Server, body string) int {
	r := httptest.NewRequest(http.MethodPut, "/api/v1/bots/meta-bot", strings.NewReader(body))
	r.SetPathValue("name", "meta-bot")
	w := httptest.NewRecorder()
	s.handleBotsPut(w, r)
	return w.Code
}

// Two metadata edits of one bot at once both land: each is patched under
// the authoring lock over what the other wrote — a disable among them is
// never lost to an edit patched from the manifest read before it.
func TestTwoManifestEditsAtOnceBothLand(t *testing.T) {
	const rounds = 40
	for i := 0; i < rounds; i++ {
		work := t.TempDir()
		writeFiles(t, work, map[string]string{
			"bots/meta-bot/main.bot":      metaBakedBot,
			"bots/meta-bot/manifest.yaml": "name: meta-bot\nversion: 1.0.0\ndescription: start\nenabled: true\n",
		})
		s := newOrgTestServer(t)
		s.cfg.WorkDir = work
		desc := "edited-" + strconv.Itoa(i)
		var codes [2]int
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; codes[0] = putManifest(s, `{"enabled":false}`) }()
		go func() { defer wg.Done(); <-start; codes[1] = putManifest(s, `{"description":"`+desc+`"}`) }()
		close(start)
		wg.Wait()
		if codes != [2]int{http.StatusOK, http.StatusOK} {
			t.Fatalf("round %d: the two edits answered %v, want both 200", i, codes)
		}
		m, err := bundle.LoadManifest(filepath.Join(work, "bots/meta-bot/manifest.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if m.IsEnabled() || m.Description != desc {
			raw, _ := os.ReadFile(filepath.Join(work, "bots/meta-bot/manifest.yaml"))
			t.Fatalf("round %d: both edits answered 200 but the manifest holds enabled=%v description=%q (want false, %q):\n%s", i, m.IsEnabled(), m.Description, desc, raw)
		}
		bot, found, err := s.resolvePipelineBot(context.Background(), "", "meta-bot")
		if err != nil || !found {
			t.Fatalf("round %d: resolve: found=%v err=%v", i, found, err)
		}
		bot.Launch.Cleanup()
		if bot.Enabled {
			t.Fatalf("round %d: the card door reads the disabled bot enabled", i)
		}
	}
}

// A reader that takes no lock — every listing reads manifest.yaml — never
// meets the file empty or absent while the studio edits it: an empty
// manifest reads with its defaults (enabled), an absent one makes the
// bundle's main.bot a loose bot. The edit replaces the file in one rename.
func TestAManifestEditNeverShowsAReaderAnEmptyOrAbsentManifest(t *testing.T) {
	work := t.TempDir()
	writeFiles(t, work, map[string]string{
		"bots/meta-bot/main.bot":      metaBakedBot,
		"bots/meta-bot/manifest.yaml": "name: meta-bot\nversion: 1.0.0\ndescription: start\nenabled: false\n",
	})
	s := newOrgTestServer(t)
	s.cfg.WorkDir = work
	path := filepath.Join(work, "bots/meta-bot/manifest.yaml")
	var reads, absent, enabled atomic.Int64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			b, err := os.ReadFile(path)
			reads.Add(1)
			if errors.Is(err, fs.ErrNotExist) {
				absent.Add(1)
				continue
			}
			if err != nil {
				continue
			}
			if m, err := bundle.DecodeManifest(b, "manifest.yaml"); err == nil && m.IsEnabled() {
				enabled.Add(1)
			}
		}
	}()
	for i := 0; i < 300; i++ {
		if code := putManifest(s, `{"description":"edit-`+strconv.Itoa(i)+`"}`); code != http.StatusOK {
			close(done)
			wg.Wait()
			t.Fatalf("edit %d = %d", i, code)
		}
	}
	close(done)
	wg.Wait()
	t.Logf("reads=%d absent=%d read-enabled=%d during 300 edits", reads.Load(), absent.Load(), enabled.Load())
	if absent.Load() > 0 || enabled.Load() > 0 {
		t.Fatalf("a reader met manifest.yaml absent %d times and read the disabled bot enabled %d times while it was edited", absent.Load(), enabled.Load())
	}
}

// replaceFile writes only over the bytes its write was decided on: a file
// another writer replaced outside the lock is a 409 and keeps its bytes; a
// file created since an absence was read is a 409 too.
func TestReplaceFileWritesOnlyOverTheBytesItWasDecidedOn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	writeFiles(t, dir, map[string]string{"manifest.yaml": "name: a\n"})
	decided := storedAt([]byte("name: a\n"))
	theirs := "name: a\ndescription: another writer's\n"
	writeFiles(t, dir, map[string]string{"manifest.yaml": theirs})
	locks, err := acquireAuthoringLocalLocks(context.Background(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	err = locks[0].replaceFile(storedCheck{name: "manifest.yaml", decided: decided}, []byte("name: a\ndescription: mine\n"), nil)
	closeAuthoringLocalLocks(locks)
	var refused *storedFileRefusal
	if !errors.As(err, &refused) || refused.status != http.StatusConflict {
		t.Fatalf("a replace decided on bytes another writer replaced = %v, want the 409", err)
	}
	assertFiles(t, dir, map[string]string{"manifest.yaml": theirs}, "after the refused replace")

	fresh := filepath.Join(dir, "new.yaml")
	writeFiles(t, dir, map[string]string{"new.yaml": "created meanwhile\n"})
	locks, err = acquireAuthoringLocalLocks(context.Background(), []string{fresh})
	if err != nil {
		t.Fatal(err)
	}
	err = locks[0].replaceFile(storedCheck{name: "new.yaml", decided: storedFile{}}, []byte("mine\n"), nil)
	closeAuthoringLocalLocks(locks)
	if !errors.As(err, &refused) || refused.status != http.StatusConflict {
		t.Fatalf("a creation decided on an absence another writer filled = %v, want the 409", err)
	}
	assertFiles(t, dir, map[string]string{"new.yaml": "created meanwhile\n"}, "after the refused creation")
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.replace-*")); len(left) > 0 {
		t.Fatalf("a refused replace left its staged candidate behind: %v", left)
	}
}

// The stored-file check reads no more of the stored file than the bytes the
// write was decided on, and one byte: a longer file is another file (409)
// without being read whole.
func TestTheStoredCheckReadsNoMoreThanTheDecidedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256 << 20); err != nil { // sparse
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	locks, err := acquireAuthoringLocalLocks(context.Background(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err = locks[0].checkStored(storedCheck{name: "big.log", decided: storedAt([]byte("small\n"))})
	runtime.ReadMemStats(&after)
	alloc := (after.TotalAlloc - before.TotalAlloc) >> 20
	var refused *storedFileRefusal
	if !errors.As(err, &refused) || refused.status != http.StatusConflict {
		t.Fatalf("a check of a 256 MiB file against 6 decided bytes = %v, want the 409", err)
	}
	if alloc > 16 {
		t.Fatalf("the check allocated %d MiB: it read the stored file past the decided bytes", alloc)
	}
}

// putRunFile sends the run file editor's save of path, without a digest.
func putRunFile(t *testing.T, baseURL, path, content string) int {
	t.Helper()
	body, _ := json.Marshal(saveRunFileRequest{Path: path, Content: content})
	req, err := http.NewRequest(http.MethodPut, baseURL+"/api/runs/edit-run/files/content", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// The run file editor holds the file a path RESOLVES to to the reader's
// refusal, whatever the request spells: "x.bot/.", "x.bot/" and a link named
// notes.txt all name x.bot, a workflow file the reader refuses as stored.
func TestARunFileEditChecksTheFileItsPathResolvedTo(t *testing.T) {
	stored := "workflow w:\n  entry: done\n\nprompt big:\n" + strings.Repeat("  a long prompt line\n", 300)
	mine := "workflow w:\n  entry: done\n"
	for _, path := range []string{"x.bot", "x.bot/.", "x.bot/", "notes.txt"} {
		t.Run(path, func(t *testing.T) {
			srv, hs := newTestServer(t)
			dir := initRepo(t)
			writeFiles(t, dir, map[string]string{"x.bot": stored})
			if err := os.Symlink("x.bot", filepath.Join(dir, "notes.txt")); err != nil {
				t.Fatal(err)
			}
			seedRunWithWorkDir(t, srv, "edit-run", dir, true)
			t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "x.bot", mine)+20))
			if code := putRunFile(t, hs.URL, path, mine); code != http.StatusConflict {
				t.Errorf("an undigested save through %q over a .bot the reader refuses = %d, want 409 (the digest rule)", path, code)
			}
			assertFiles(t, dir, map[string]string{"x.bot": stored}, "after the save through "+path)
		})
	}
}

// The run file save reads the stored file no further than the editor's
// limit: a file past it is no file the editor shows (the GET's 413), so the
// save answers 413 too, without reading it whole, and writes nothing.
func TestARunFileSaveNeverReadsAStoredFilePastTheEditorsLimit(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	big := filepath.Join(dir, "big.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256 << 20); err != nil { // sparse
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	seedRunWithWorkDir(t, srv, "edit-run", dir, true)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	code := putRunFile(t, hs.URL, "big.log", "small\n")
	runtime.ReadMemStats(&after)
	alloc := (after.TotalAlloc - before.TotalAlloc) >> 20
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("a save over a 256 MiB stored file = %d, want 413", code)
	}
	if alloc > 64 {
		t.Errorf("the save allocated %d MiB: it read the stored file past the editor's limit", alloc)
	}
	if info, err := os.Stat(big); err != nil || info.Size() != 256<<20 {
		t.Errorf("the stored file changed: %v %v", info, err)
	}
}

// The studio's "New bot" creates its files and never replaces one: a Save
// As of the new bot's main.bot that lands while the scaffold is past its
// absence check keeps its bytes — one of the two writers gets a conflict,
// never both a success and a lost file — and a New bot refused that way
// leaves nothing of itself: no manifest beside the Save As's main.bot, no
// bot to list or route, no staging directory.
func TestTheNewBotScaffoldNeverReplacesTheMainASaveAsCreated(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "0")
	seedSrc := unitFixtureNodes + "\nworkflow w:\n  entry: worker\n  worker -> done\n"
	raced := 0
	for i := 0; i < 3; i++ {
		work := t.TempDir()
		writeFiles(t, work, map[string]string{"src/seed.bot": seedSrc})
		if err := os.MkdirAll(filepath.Join(work, "bots"), 0o755); err != nil {
			t.Fatal(err)
		}
		s := newOrgTestServer(t)
		s.cfg.WorkDir = work
		_, opened := openPath(t, s, "src/seed.bot")
		if !opened.Bindable {
			t.Fatalf("seed does not open bindable")
		}
		dir := filepath.Join(work, "bots", "race-bot")
		// The Save As publishes once the scaffold stages its bundle: the
		// scaffold is past its absence check and inside its write.
		staging := filepath.Join(work, "bots", ".race-bot.scaffold-*")
		authoringStepHook = func(stage string) error {
			if stage != "publish" {
				return nil
			}
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				staged, _ := filepath.Glob(staging)
				if len(staged) > 0 {
					return nil
				}
				time.Sleep(time.Millisecond)
			}
			return errors.New("the scaffold never staged its bundle")
		}
		createDone := make(chan *httptest.ResponseRecorder, 1)
		// A long mission keeps the scaffold rendering while the Save As
		// reads the main's absence.
		spec, _ := json.Marshal(map[string]string{"slug": "race-bot", "instructions": strings.Repeat("Do the thing carefully, step by step.\n", 20000)})
		go func() {
			rr := httptest.NewRecorder()
			s.handleBotCreate(rr, httptest.NewRequest(http.MethodPost, "/api/v1/bots", bytes.NewReader(spec)))
			createDone <- rr
		}()
		waitForStack(t, "botscaffold.Scaffold")
		body, _ := json.Marshal(saveFileRequest{Path: "bots/race-bot/main.bot", Document: opened.Document, CreateOnly: true})
		saved := httptest.NewRecorder()
		s.handleSaveFile(saved, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body)))
		created := <-createDone
		authoringStepHook = nil
		var resp saveFileResponse
		_ = json.Unmarshal(saved.Body.Bytes(), &resp)
		got, _ := os.ReadFile(filepath.Join(dir, "main.bot"))
		if saved.Code == http.StatusOK {
			raced++
			if string(got) != resp.Source {
				t.Fatalf("round %d: the Save As answered 200 and the New bot %d, and main.bot holds %d bytes, not the %d the Save As wrote", i, created.Code, len(got), len(resp.Source))
			}
			if created.Code != http.StatusConflict {
				t.Fatalf("round %d: the New bot answered %d over the main the Save As created, want 409: %s", i, created.Code, created.Body.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "manifest.yaml")); err == nil {
				t.Fatalf("round %d: the refused New bot left its manifest.yaml beside the Save As's main.bot", i)
			}
			if _, listed, _ := s.findBot("race-bot"); listed {
				t.Fatalf("round %d: the refused New bot left a bot to list", i)
			}
			if left, _ := filepath.Glob(staging); len(left) > 0 {
				t.Fatalf("round %d: the refused New bot left its staging directory: %v", i, left)
			}
		}
	}
	t.Logf("rounds where the Save As landed inside the scaffold's write: %d of 3", raced)
}

// waitForStack waits until a goroutine's stack holds fn — a condition,
// never a tick.
func waitForStack(t *testing.T, fn string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	buf := make([]byte, 1<<22)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), fn) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no goroutine reached %s", fn)
}

// replaceFile checks the stored file after its candidate is staged — written
// and fsynced — so a writer outside the lock that lands while the candidate
// is staged (an operator's editor disabling the bot) makes the edit a 409,
// its bytes kept; and a creation renames without replacing, so a file
// created after the check is a 409 too, never replaced.
func TestReplaceFileChecksAfterItsCandidateIsStaged(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, "bots/meta-bot/manifest.yaml")
	writeFiles(t, work, map[string]string{
		"bots/meta-bot/main.bot":      metaBakedBot,
		"bots/meta-bot/manifest.yaml": "name: meta-bot\nversion: 1.0.0\ndescription: start\nenabled: true\n",
	})
	theirs := "name: meta-bot\nversion: 1.0.1\ndescription: start\nenabled: false\n"
	authoringStepHook = func(stage string) error {
		if stage != "replace:staged" {
			return nil
		}
		return os.WriteFile(path, []byte(theirs), 0o644)
	}
	desc := "edited"
	err := patchManifestLocked(context.Background(), path, bundle.ManifestPatch{Description: &desc})
	authoringStepHook = nil
	var refused *storedFileRefusal
	if !errors.As(err, &refused) || refused.status != http.StatusConflict {
		t.Fatalf("a manifest edit while an outside writer disabled the bot = %v, want the 409", err)
	}
	assertFiles(t, work, map[string]string{"bots/meta-bot/manifest.yaml": theirs}, "after the refused edit")

	fresh := filepath.Join(work, "bots/meta-bot/new.yaml")
	locks, err := acquireAuthoringLocalLocks(context.Background(), []string{fresh})
	if err != nil {
		t.Fatal(err)
	}
	created := "created after the check\n"
	err = locks[0].replaceFile(storedCheck{name: "new.yaml", decided: storedFile{}}, []byte("mine\n"), func(string) {
		if werr := os.WriteFile(fresh, []byte(created), 0o644); werr != nil {
			t.Error(werr)
		}
	})
	closeAuthoringLocalLocks(locks)
	if !errors.As(err, &refused) || refused.status != http.StatusConflict {
		t.Fatalf("a creation whose name another writer took after the check = %v, want the 409", err)
	}
	assertFiles(t, work, map[string]string{"bots/meta-bot/new.yaml": created}, "after the refused creation")
	if left, _ := filepath.Glob(filepath.Join(work, "bots/meta-bot/.*.replace-*")); len(left) > 0 {
		t.Fatalf("a refused replace left its staged candidate behind: %v", left)
	}
}

// The run file editor writes in place, so a file with other names — a hard
// link notes.txt to x.bot, a workflow file the reader refuses — changes
// under every name: a save that presents no digest of the text it replaces
// is refused (409) and x.bot keeps its bytes; one that presents the digest
// holds those bytes and replaces them.
func TestARunFileSaveOverAFileWithOtherNamesNeedsItsDigest(t *testing.T) {
	stored := "workflow w:\n  entry: done\n\nprompt big:\n" + strings.Repeat("  a long prompt line\n", 300)
	mine := "workflow w:\n  entry: done\n"
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	writeFiles(t, dir, map[string]string{"x.bot": stored})
	if err := os.Link(filepath.Join(dir, "x.bot"), filepath.Join(dir, "notes.txt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	seedRunWithWorkDir(t, srv, "edit-run", dir, true)
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(spentAlone(t, "x.bot", mine)+20))
	if code := putRunFile(t, hs.URL, "notes.txt", mine); code != http.StatusConflict {
		t.Fatalf("an undigested save through notes.txt, a hard link to a .bot the reader refuses = %d, want 409", code)
	}
	assertFiles(t, dir, map[string]string{"x.bot": stored}, "after the undigested save through notes.txt")
	body, _ := json.Marshal(saveRunFileRequest{Path: "notes.txt", Content: mine, StoredSHA256: contentSHA256(stored)})
	req, err := http.NewRequest(http.MethodPut, hs.URL+"/api/runs/edit-run/files/content", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a save through notes.txt presenting the stored digest = %d, want 200", res.StatusCode)
	}
	assertFiles(t, dir, map[string]string{"x.bot": mine}, "after the digested save")
}
