package cli

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
)

// TestABotFileIsReplacedOnlyOverTheBytesItsWriterRead is the contract the
// server's editors have had since #1690 and the CLI writers had not: a
// rewrite lands only over the bytes it was decided on. Each case is what a
// save by the studio, the assistant or the unit editor does between a
// command's read and its write.
func TestABotFileIsReplacedOnlyOverTheBytesItsWriterRead(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		there     string // "" = the file is not there
		decidedOn []byte
		wantWrite bool
		why       string
	}{
		{name: "the bytes are the ones it read", there: "mine\n", decidedOn: []byte("mine\n"), wantWrite: true},
		{name: "another writer replaced it since", there: "theirs\n", decidedOn: []byte("mine\n"), why: "replaced it since it was read"},
		{name: "another writer created it since an absence was read", there: "theirs\n", decidedOn: nil, why: "created it since it was read as absent"},
		{name: "another writer deleted it since", there: "", decidedOn: []byte("mine\n"), why: "deleted it since it was read"},
		{name: "a creation over an absence", there: "", decidedOn: nil, wantWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.bot")
			if tc.there != "" {
				if err := os.WriteFile(path, []byte(tc.there), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := replaceBotFile(ctx, path, tc.decidedOn, []byte("written\n"), 0o644)
			body, readErr := os.ReadFile(path)
			if tc.wantWrite {
				if err != nil {
					t.Fatalf("replace = %v, want the write", err)
				}
				if readErr != nil || string(body) != "written\n" {
					t.Fatalf("file = %q (%v), want the written bytes", body, readErr)
				}
				return
			}
			if !errors.Is(err, ErrBotFileChanged) {
				t.Fatalf("replace = %v, want ErrBotFileChanged", err)
			}
			if !strings.Contains(err.Error(), tc.why) {
				t.Errorf("the refusal does not say what happened (%q): %v", tc.why, err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the refusal does not name the file: %v", err)
			}
			if tc.there == "" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("a refused write created the file anyway: %v", err)
				}
				return
			}
			if readErr != nil || string(body) != tc.there {
				t.Errorf("the other writer's bytes were replaced: %q (%v)", body, readErr)
			}
		})
	}
}

// TestAReplacementRefusesWhileASaveHoldsTheLock is the mutual exclusion the
// stale check alone cannot give: a save in flight has decided its bytes and
// not written them yet, so there is nothing on disk to compare. The lock is
// what makes the two writers take turns.
func TestAReplacementRefusesWhileASaveHoldsTheLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.bot")
	theirs := "the editor's\n"
	if err := os.WriteFile(path, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := authoringlock.Take(context.Background(), path)
	if err != nil {
		t.Fatalf("stand in for an editor's save: %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = replaceBotFile(ctx, path, []byte(theirs), []byte("mine\n"), 0o644)
	if err == nil {
		t.Fatal("a replacement went through while a save held the lock")
	}
	if !strings.Contains(err.Error(), "authoring lock was not taken") {
		t.Errorf("the refusal does not name the lock as its cause: %v", err)
	}
	if errors.Is(err, ErrBotFileChanged) {
		t.Errorf("a refused TAKE is not a changed file: %v", err)
	}
	if body, _ := os.ReadFile(path); string(body) != theirs {
		t.Errorf("the file moved although nothing was written: %q", body)
	}
}

// TestFmtRefusesAFileASaveHoldsAndFormatsTheRest walks the PUBLIC path an
// operator reaches — `iterion fmt -w` over a tree — and holds it to the two
// halves that matter: the file being saved keeps its bytes and is reported
// as refused, and its neighbours are formatted all the same. The wait on
// the held lock is bounded by the CONTEXT the caller hands RunFmt (#2839):
// the command's own context, which cobra cancels on SIGINT — an operator's
// Ctrl-C shortens it. This test stands in for it with a timeout.
func TestFmtRefusesAFileASaveHoldsAndFormatsTheRest(t *testing.T) {
	inTempWorkspace(t)
	held := writeBot(t, "f/held.bot", looseBot)
	free := writeBot(t, "f/free.bot", looseBot)
	lock, err := authoringlock.Take(context.Background(), held)
	if err != nil {
		t.Fatalf("stand in for an editor's save: %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	p, out := humanPrinter()
	// The command runs in a goroutine so a wait no context bounds fails this
	// test's own assertion instead of the harness's global timeout: a
	// one-shot CLI that sits forever behind an editor is the other half of
	// what the context is for. The deferred release ends the goroutine.
	type outcome struct {
		res FmtResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := RunFmt(ctx, FmtOptions{Paths: []string{"f"}, Printer: p})
		done <- outcome{res, err}
	}()
	var res FmtResult
	select {
	case got := <-done:
		res, err = got.res, got.err
	case <-time.After(10 * time.Second):
		t.Fatal("fmt did not come back within 10s though its caller's context ends the wait at 300ms: a command can sit behind an editor's save for as long as the save lasts")
	}
	if !errors.Is(err, ErrFmtRefused) {
		t.Fatalf("fmt over a save in flight = %v, want ErrFmtRefused\n%s", err, out.String())
	}
	if len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "authoring lock was not taken") {
		t.Fatalf("refusals = %v, want the held file named with its cause", res.Refused)
	}
	if !strings.Contains(res.Refused[0], "held.bot") {
		t.Errorf("the refusal does not name the file: %v", res.Refused[0])
	}
	if body, _ := os.ReadFile(held); string(body) != looseBot {
		t.Errorf("the file a save held was formatted anyway:\n%s", body)
	}
	if body, _ := os.ReadFile(free); string(body) == looseBot {
		t.Errorf("one file being saved stopped its neighbour from being formatted")
	}
	if len(res.Files) != 1 || res.Files[0].Path != free || !res.Files[0].Written {
		t.Errorf("written set = %+v, want the free file alone", res.Files)
	}
}

// botFileWriterAllowed are the files that may call writeFileAtomic without
// the authoring lock: the chokepoint itself, which takes the lock around it,
// and the schedule wrapper — a shell script this CLI generates under the
// operator's state directory, which no authoring editor ever opens and which
// is not a file of a bot.
func botFileWriterAllowed() map[string]bool {
	return map[string]bool{
		"write_bot_file.go": true,
		"schedule_path.go":  true,
	}
}

// TestEveryWriterOfABotSFileGoesThroughTheLockedChokepoint keeps the fix at
// its class. #2689's CLI half was five writers that read and replaced a
// stored bot file outside the lock; one chokepoint now takes it, and the
// next writer added beside them must go through it or name itself here.
func TestEveryWriterOfABotSFileGoesThroughTheLockedChokepoint(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information: the walk cannot find its own package")
	}
	pkgDir := filepath.Dir(self)
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	allowed := botFileWriterAllowed()
	offenders := map[string][]int{}
	fset := token.NewFileSet()
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(pkgDir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			id, isIdent := call.Fun.(*ast.Ident)
			if !isIdent || id.Name != "writeFileAtomic" {
				return true
			}
			seen++
			if !allowed[name] {
				offenders[name] = append(offenders[name], fset.Position(call.Pos()).Line)
			}
			return true
		})
	}
	if seen == 0 {
		t.Fatal("the walk found no call to writeFileAtomic at all: it is looking at the wrong thing")
	}
	if len(offenders) > 0 {
		names := make([]string, 0, len(offenders))
		for name := range offenders {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			t.Errorf("%s calls writeFileAtomic outside the authoring lock at line(s) %v: a writer of a bot's file calls replaceBotFile, or names itself in botFileWriterAllowed with its reason", name, offenders[name])
		}
	}
}
