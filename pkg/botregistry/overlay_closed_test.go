package botregistry

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// overlayWorkspace is a workspace holding two loose bots and an overlay
// body (none when empty).
func overlayWorkspace(t *testing.T, overlay string) string {
	t.Helper()
	work := t.TempDir()
	for _, b := range []string{"secret-bot", "other-bot"} {
		writeFile(t, filepath.Join(work, "bots", b+".bot"), "## ---\n## name: "+b+"\n## ---\nagent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n")
	}
	if overlay != "" {
		writeFile(t, filepath.Join(work, ".iterion", "bot-overrides.yaml"), overlay)
	}
	return work
}

// The operator's decisions come first in the file, as a hand-edit or the
// Catalog manager leaves them; a tail puts the file past a bound of its
// reader without touching them.
const overlayDecisions = "bots:\n  secret-bot:\n    enabled: false\n  other-bot:\n    enabled: false\n"

func enabledOf(entries []Entry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		out[e.Name] = e.Enabled
	}
	return out
}

// An overlay its reader refuses — dense past the read budget, or past the
// size cap — holds decisions nobody read: every listing that routes,
// launches or dispatches by Entry.Enabled is refused with it, never answered
// as if there were no overlay (which re-enabled every bot the operator
// disabled, r4b F1). A listing that opts past it answers every bot disabled
// and the refusal said; a name check reads past it, since a name is taken or
// free whatever the overlay decides.
func TestAnOverlayItsReaderRefusesRoutesNoBot(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "200")
	for _, c := range []struct {
		name    string
		overlay string
		refusal bool
	}{
		{"dense past the read budget", overlayDecisions + "pad: {" + strings.Repeat("a,", 300) + "a}\n", true},
		{"past the size cap", overlayDecisions + "# " + strings.Repeat("x", overridesMaxBytes) + "\n", true},
		{"not YAML", overlayDecisions + "\t\tnot: [yaml\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			work := overlayWorkspace(t, c.overlay)
			opts := ListOptions{Paths: DefaultPaths(work), Workdir: work}
			check := func(door string, err error) {
				t.Helper()
				var refused *OverlayRefusal
				if err == nil || (c.refusal && !errors.As(err, &refused)) {
					t.Errorf("%s over the overlay answered %v, want its refusal", door, err)
				}
			}
			entries, err := List(opts)
			check("List", err)
			if entries != nil {
				t.Errorf("List answered entries %v past the refusal", enabledOf(entries))
			}
			_, err = ListWithSchema(opts)
			check("ListWithSchema", err)
			_, _, err = ListWithSchemaDiagnostics(opts)
			check("ListWithSchemaDiagnostics", err)
			_, _, err = FindByName(opts, "secret-bot")
			check("FindByName", err)
			if !c.refusal {
				return
			}
			past := opts
			past.ListPastAnUnreadOverlay = true
			entries, diags, err := ListWithDiagnostics(past)
			if err != nil {
				t.Fatalf("a listing opted past the refusal: %v", err)
			}
			if got := enabledOf(entries); len(got) != 2 || got["secret-bot"] || got["other-bot"] {
				t.Errorf("listed past the refusal: %v, want both bots disabled", got)
			}
			if len(diags) != 1 || diags[0].Kind != DiscoveryErrorOverlay || !strings.Contains(diags[0].Error, "listed disabled") {
				t.Errorf("diagnostics %+v, want the overlay's refusal and what it does to the listing", diags)
			}
			if err := EnsureNameFree(opts, "secret-bot"); !errors.Is(err, ErrNameTaken) {
				t.Errorf("a taken name past the refusal: %v, want ErrNameTaken", err)
			}
			if err := EnsureNameFree(opts, "fresh-bot"); err != nil {
				t.Errorf("a free name past the refusal: %v", err)
			}
		})
	}
	// Controls: no overlay, every bot enabled; the decisions read, each
	// honoured.
	if got := enabledOf(mustList(t, overlayWorkspace(t, ""))); !got["secret-bot"] || !got["other-bot"] {
		t.Errorf("no overlay: %v, want both enabled", got)
	}
	if got := enabledOf(mustList(t, overlayWorkspace(t, overlayDecisions))); got["secret-bot"] || got["other-bot"] {
		t.Errorf("the decisions read: %v, want both disabled", got)
	}
}

func mustList(t *testing.T, work string) []Entry {
	t.Helper()
	entries, err := List(ListOptions{Paths: DefaultPaths(work), Workdir: work})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// waitStack waits until a goroutine's stack holds every one of marks — a
// condition, never a tick.
func waitStack(t *testing.T, marks ...string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	buf := make([]byte, 1<<22)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
	goroutines:
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			for _, m := range marks {
				if !strings.Contains(g, m) {
					continue goroutines
				}
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no goroutine reached %v", marks)
}

func overlayOf(t *testing.T, work string) map[string]bool {
	t.Helper()
	ov, err := LoadOverrides(work)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for name, bo := range ov.Bots {
		if bo.Enabled != nil {
			out[name] = *bo.Enabled
		}
	}
	return out
}

// Two toggles at once (two clicks of the Catalog manager, two replicas)
// each rewrite the whole file from a reading of it: one that read before the
// other wrote would rewrite the file without the other's decision (r4b F3,
// 20 rounds out of 20). The reading and the write are one critical section:
// the first toggle holds it across its read — here until the second is
// waiting on it — and both decisions land.
func TestTwoOverlayTogglesAtOnceBothLand(t *testing.T) {
	work := t.TempDir()
	writeFile(t, filepath.Join(work, ".iterion", "bot-overrides.yaml"), "bots:\n  first-bot:\n    enabled: false\n")
	first := make(chan struct{})
	var reached atomic.Int32
	overlayWriteHook = func() {
		if reached.Add(1) != 1 {
			return
		}
		close(first)
		// Held past this reading until the other toggle is either waiting
		// to enter the section, or — were the section open — past its own
		// reading of the same file.
		deadline := time.Now().Add(20 * time.Second)
		buf := make([]byte, 1<<22)
		for reached.Load() < 2 && time.Now().Before(deadline) {
			n := runtime.Stack(buf, true)
			for _, g := range strings.Split(string(buf[:n]), "\n\n") {
				if strings.Contains(g, "SetOverlayEnabled") && strings.Contains(g, "flock.(*Flock).TryLockContext") {
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		if reached.Load() < 2 {
			t.Error("the second toggle never reached the overlay's critical section")
		}
	}
	t.Cleanup(func() { overlayWriteHook = nil })
	off := false
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(1)
	go func() { defer wg.Done(); errs <- SetOverlayEnabled(context.Background(), work, "bot-a", &off) }()
	<-first
	wg.Add(1)
	go func() { defer wg.Done(); errs <- SetOverlayEnabled(context.Background(), work, "bot-b", &off) }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := overlayOf(t, work); len(got) != 3 || got["bot-a"] || got["bot-b"] || got["first-bot"] {
		t.Fatalf("after two toggles at once the overlay holds %v, want first-bot, bot-a and bot-b disabled", got)
	}
}

// Another process writing the same workspace takes the same lock file: a
// toggle waits for it, and reads the overlay that process left — never one
// from before it.
func TestAnOverlayToggleWaitsForAnotherProcesssLock(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, ".iterion", "bot-overrides.yaml")
	writeFile(t, path, "bots:\n  first-bot:\n    enabled: false\n")
	other := flock.New(overlayLockPath(work))
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}
	off := false
	done := make(chan error, 1)
	go func() { done <- SetOverlayEnabled(context.Background(), work, "bot-a", &off) }()
	waitStack(t, "SetOverlayEnabled", "flock")
	// The other process writes its decision while it holds the lock.
	if err := os.WriteFile(path, []byte("bots:\n  first-bot:\n    enabled: false\n  theirs:\n    enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := other.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := overlayOf(t, work); len(got) != 3 || got["theirs"] || got["bot-a"] {
		t.Fatalf("the overlay holds %v, want the other process's decision and the toggle's", got)
	}
}

// TestEveryUnreadOverlayOptInIsListed: a listing past a workspace overlay
// its reader refused is an opt-in (ListOptions.ListPastAnUnreadOverlay), and
// every production site that sets it is listed here with what it reads of
// the listing — a surface that shows the refusal beside it, or a reader of
// nothing the overlay decides. A door that routes, launches or dispatches by
// Entry.Enabled never opts in; a new site fails until it says why, a second
// one in a listed function is a new occurrence, and a stale entry fails too.
func TestEveryUnreadOverlayOptInIsListed(t *testing.T) {
	optIns := map[string]string{
		"pkg/botregistry/registry.go EnsureNameFree #1":                   "a name is taken or free whatever the overlay decides of its visibility",
		"pkg/server/bots_tenant.go mergedBotEntries #1":                   "the studio's gallery: every bot listed disabled, the refusal among the diagnostics it renders (DiscoveryErrorBanners)",
		"pkg/server/bot_override_staleness.go newBakedCatalogResolver #1": "the override-staleness projection reads versions alone, which no overlay decides",
		"pkg/server/bot_sources_routes.go platformPushWarnings #1":        "the baked manifest's invocations, which no overlay decides",
		"pkg/server/bot_resolver.go effectiveEntriesWithSchema #1":        "the launch lanes' catalog read — webhook hand-off's consumers, the review gate's var default, the forge command map and its pause notice, the trigger spine's derivation: invocations, schemas and manifests, which no overlay decides; every bot listed disabled keeps the doors that DO read visibility (card launch, command discovery) routing none, and the launch itself resolves without the overlay",
	}
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// By path from the root, never by a directory's name.
			if rel == "vendor" || rel == "studio" || rel == "node_modules" || (strings.HasPrefix(rel, ".") && rel != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, perr := goparser.ParseFile(token.NewFileSet(), path, nil, goparser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			n := 0
			note := func() {
				n++
				key := fmt.Sprintf("%s %s #%d", rel, fd.Name.Name, n)
				seen[key] = true
				if _, ok := optIns[key]; !ok {
					offenders = append(offenders, key)
				}
			}
			ast.Inspect(fd.Body, func(node ast.Node) bool {
				switch x := node.(type) {
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "ListPastAnUnreadOverlay" {
							note()
						}
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "ListPastAnUnreadOverlay" {
						note()
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(offenders)
	for _, key := range offenders {
		t.Errorf("%s lists past a workspace overlay its reader refused: list it here with what it reads of the listing — or answer the refusal, as every door that routes, launches or dispatches does", key)
	}
	var stale []string
	for key := range optIns {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("%s is listed as an unread-overlay opt-in but no site answers it: remove the entry", key)
	}
}

// A toggle's wait for the overlay's lock ends: a holder that never lets go
// — another process stuck inside its section — fails the toggle with the
// caller's context, or after overlayLockWait when the caller sets no
// earlier end, and nothing is written. It never hangs the request.
func TestAnOverlayToggleGivesUpOnALockNobodyReleases(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, ".iterion", "bot-overrides.yaml")
	stored := "bots:\n  first-bot:\n    enabled: false\n"
	writeFile(t, path, stored)
	other := flock.New(overlayLockPath(work))
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Unlock() })
	off := false
	toggle := func(ctx context.Context) error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- SetOverlayEnabled(ctx, work, "bot-a", &off) }()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the toggle still waits on a lock nobody releases after 10s")
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := toggle(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a toggle whose context ends while the lock is held = %v, want the context's deadline", err)
	}
	saved := overlayLockWait
	overlayLockWait = 200 * time.Millisecond
	t.Cleanup(func() { overlayLockWait = saved })
	if err := toggle(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a toggle with no deadline of its own = %v, want overlayLockWait's", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != stored {
		t.Fatalf("the overlay changed under a toggle that never held the lock: %q %v", got, err)
	}
}

// allocDuring is what fn allocated, in bytes.
func allocDuring(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Every catalogue listing reads the overlay, and a decoding at its bound
// builds hundreds of MiB: a listing that finds the bytes it decoded last
// time decodes nothing, and listings that meet new bytes at once decode
// them once between them.
func TestAListingOfAnUnchangedOverlayDecodesItOnce(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "")
	work := overlayWorkspace(t, overlayDecisions+"pad: ["+strings.Repeat("a,", 200_000)+"a]\n")
	opts := ListOptions{Paths: DefaultPaths(work), Workdir: work}
	list := func() {
		t.Helper()
		entries, _, err := ListWithDiagnostics(opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := enabledOf(entries); got["secret-bot"] || got["other-bot"] {
			t.Fatalf("the listing reads the overlay's decisions as %v, want both bots disabled", got)
		}
	}
	first := allocDuring(list)
	again := allocDuring(list)
	t.Logf("first listing allocated %d KiB, the next %d KiB", first>>10, again>>10)
	if again > first/4 {
		t.Fatalf("a listing of the unchanged overlay allocated %d KiB against the first's %d KiB: it decoded the overlay again", again>>10, first>>10)
	}
	// New bytes, four listings at once: one decoding between them.
	writeFile(t, filepath.Join(work, ".iterion", "bot-overrides.yaml"), overlayDecisions+"pad: ["+strings.Repeat("b,", 200_000)+"b]\n")
	together := allocDuring(func() {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); list() }()
		}
		wg.Wait()
	})
	t.Logf("four listings of new bytes allocated %d KiB together", together>>10)
	if together > first*2 {
		t.Fatalf("four listings of new bytes allocated %d KiB, more than two decodings (%d KiB each): they decoded them each", together>>10, first>>10)
	}
}

// The overlay is answered for its BYTES, under the read budget in force: a
// rewrite of the same size in place — the same inode, a modification time
// a coarse clock may not move — is read anew, and a budget raised past a
// refused overlay reads it.
func TestTheOverlayIsReadAgainWhenItsBytesOrTheBudgetChange(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "")
	work := overlayWorkspace(t, "bots:\n  secret-bot:\n    enabled: false\n")
	opts := ListOptions{Paths: DefaultPaths(work), Workdir: work}
	enabled := func() map[string]bool {
		t.Helper()
		entries, _, err := ListWithDiagnostics(opts)
		if err != nil {
			t.Fatal(err)
		}
		return enabledOf(entries)
	}
	if got := enabled(); got["secret-bot"] || !got["other-bot"] {
		t.Fatalf("before the rewrite: %v, want secret-bot disabled", got)
	}
	path := filepath.Join(work, ".iterion", "bot-overrides.yaml")
	if err := os.WriteFile(path, []byte("bots:\n  other-bot:\n     enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := enabled(); !got["secret-bot"] || got["other-bot"] {
		t.Fatalf("after a same-size rewrite in place: %v, want other-bot disabled and secret-bot back", got)
	}
	dense := "bots:\n  secret-bot:\n    enabled: false\npad: {" + strings.Repeat("a,", 300) + "a}\n"
	writeFile(t, path, dense)
	t.Setenv(parser.ReadBudgetEnv, "200")
	var refused *OverlayRefusal
	if _, _, err := ListWithDiagnostics(opts); !errors.As(err, &refused) {
		t.Fatalf("a listing past the budget = %v, want the overlay's refusal", err)
	}
	t.Setenv(parser.ReadBudgetEnv, "")
	entries, _, err := ListWithDiagnostics(opts)
	if err != nil || enabledOf(entries)["secret-bot"] {
		t.Fatalf("with the budget raised past it the listing = %v, %v — want the overlay read, secret-bot disabled", enabledOf(entries), err)
	}
}
