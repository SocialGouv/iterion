package store

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// StoreDirName is the conventional directory name for an iterion run
// store, both as the project-local opt-in marker and as the leaf of
// the global iterion data root (~/.iterion).
const StoreDirName = ".iterion"

// ResolveStoreDir picks the run-store directory shared by the CLI and
// the studio.
//
// Resolution order:
//  1. Explicit override (--store-dir, cfg.StoreDir) wins.
//  2. Project-local opt-in: if <start>/.iterion exists as a directory,
//     use it. Created by hand when the operator wants the run state
//     versioned with (or merely adjacent to) the repo.
//  3. Default: a per-project subdir of the user's global iterion data
//     dir — ~/.iterion/projects/<workdir-key>/, where <workdir-key>
//     is a deterministic encoding of the absolute workdir path. This
//     mirrors Claude Code's ~/.claude/projects/<key>/ layout: every
//     workdir gets an isolated slot, no cross-project leakage, no
//     pollution inside repos by default.
//
// Why the global default (rather than the legacy walk-up): a stray
// .iterion in any ancestor (typically ~/.iterion left over from
// running iterion once from $HOME) used to be picked up by every
// project nested under it, silently sharing run state across
// unrelated projects. The keyed layout removes that footgun by
// design.
//
// Backward compatibility: users who already have <repo>/.iterion
// keep the project-local store via the opt-in branch (step 2). Users
// who relied on the legacy walk-up to a shared parent .iterion need
// to either move/copy that directory under each project or set
// --store-dir / cfg.StoreDir explicitly.
//
// $ITERION_HOME overrides the global root for operators who want
// runs to live somewhere other than ~/.iterion (e.g. on a different
// disk, or under XDG_DATA_HOME).
func ResolveStoreDir(start, override string) string {
	if override != "" {
		return override
	}
	if start == "" {
		return StoreDirName
	}

	abs, err := filepath.Abs(start)
	if err != nil {
		return filepath.Join(start, StoreDirName)
	}

	// Project-local opt-in. To distinguish an iterion-managed store
	// from a stray `.iterion/` that some workspace tool created (e.g.
	// the whats-next bot's emit_action writing `.iterion/plans/*.md`),
	// require a marker subdir / sentinel file that the RunStore itself
	// would have written. Without this check, an LLM-created
	// `.iterion/plans/` hijacks every subsequent CLI/daemon call and
	// the operator sees an empty board / no runs. F-NEW-10.
	candidate := filepath.Join(abs, StoreDirName)
	if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
		if isManagedStore(candidate) {
			return candidate
		}
	}

	// Default: per-project slot under the global iterion data dir.
	return globalProjectStoreDir(abs)
}

// isManagedStore reports whether candidate looks like an
// iterion-managed run store, as opposed to a stray `.iterion/` left
// behind by a workspace tool. Recognises:
//   - <candidate>/runs/         (created by RunStore on first run)
//   - <candidate>/dispatcher/   (created by NativeStore on first board op)
//   - <candidate>/.iterion-store (sentinel file, write-on-init for forward
//     compatibility — RunStore can touch this on init to opt-in even when
//     no runs exist yet)
//
// Any of the three is enough; the OR semantics keep legacy stores
// (which only have runs/) working alongside dispatcher-only stores
// (which have dispatcher/ but no runs yet).
func isManagedStore(candidate string) bool {
	for _, marker := range []string{"runs", "dispatcher", ".iterion-store"} {
		if _, err := os.Stat(filepath.Join(candidate, marker)); err == nil {
			return true
		}
	}
	return false
}

// globalProjectStoreDir returns the per-project subdir of the global
// iterion data dir for absWorkDir. Layout: <iterionHome>/projects/<key>/.
func globalProjectStoreDir(absWorkDir string) string {
	return filepath.Join(GlobalIterionDataDir(), "projects", EncodeWorkDirKey(absWorkDir))
}

// GlobalIterionDataDir locates the user's iterion data dir: IterionHome,
// else <tmp>/iterion-data — the last-resort fallback when the user has no
// home dir (CI containers without HOME, etc.). Writers use it; a reader that
// trusts what it finds there resolves IterionHome instead, because the
// fallback is a shared location anyone on the host can pre-create.
func GlobalIterionDataDir() string {
	if dir, err := IterionHome(); err == nil {
		return dir
	}
	return filepath.Join(os.TempDir(), "iterion-data")
}

// InheritedIterionDataDir resolves the iterion home GlobalIterionDataDir
// names when the operator chose it, strictly from the environment the process
// INHERITED — never from a value a project `.env` filled in (see
// internal/envtrust).
//
// It answers one question: which iterion home did the OPERATOR choose?
// Callers that merely need to read or write data use
// GlobalIterionDataDir; callers deciding whether something carries the
// operator's authority — today, whether an installed plugin's servers
// may start on the launcher of a sandboxed run — use this one.
//
// Returns "" when the inherited environment names no home — ITERION_HOME
// and the home dir unset or planted — which no caller may read as
// "anywhere": it means "the operator said nothing", and a trust decision on
// it fails closed. The <tmp>/iterion-data fallback GlobalIterionDataDir's
// writers take without a home is never the answer: any local user can create
// it before the operator does.
//
// Under `go test` it resolves as IterionHome does there: an ITERION_HOME
// inherited from the operator is ignored, and the home tier names the test
// process's own home — only where production would name one, so an
// environment planted all through still names none. Under the production
// hook a home the test does not own names none, as IterionHome refuses it.
func InheritedIterionDataDir() string {
	return inheritedIterionDataDir(testing.Testing())
}

// inheritedIterionDataDir is InheritedIterionDataDir with the test binary's
// resolution switched by underTest, so a test drives the production one too.
func inheritedIterionDataDir(underTest bool) string {
	asInProduction := testHomeOff.Load()
	env := envtrust.Inherited("ITERION_HOME")
	if underTest {
		if inheritedFromTheOperator(env) {
			env = ""
		}
		if asInProduction && !homeOwnedByTheTest() {
			return ""
		}
	}
	if dir := trimSeparators(env); dir != "" {
		return absOrAsGiven(dir)
	}
	// The home tier of GlobalIterionDataDir, read from the inherited
	// environment and through the same variable os.UserHomeDir reads. A tier
	// read differently here makes the operator's own home unrecognisable, so
	// their own installed plugins quietly lose their authority: os.UserHomeDir
	// reads %USERPROFILE% on Windows, where HOME is normally unset.
	// Joined, not trimmed: HOME=/ names /.iterion, as it does there.
	home := envtrust.Inherited(homeEnvName())
	if underTest && !asInProduction && home != "" {
		return testIterionHome()
	}
	if home != "" {
		return filepath.Join(absOrAsGiven(home), StoreDirName)
	}
	return ""
}

// HomeEnvName is the variable os.UserHomeDir consults on this platform.
func HomeEnvName() string { return homeEnvName() }

func homeEnvName() string {
	switch runtime.GOOS {
	case "windows":
		return "USERPROFILE"
	case "plan9":
		return "home"
	default:
		return "HOME"
	}
}

// IterionHome resolves the iterion home the operator owns, checked in this
// order:
//  1. $ITERION_HOME — operator escape hatch
//  2. ~/.iterion    — matches the convention iterion already uses
//
// It fails when no home dir resolves. The result is absolute, so every reader
// and writer agrees on it whatever its working directory, and trailing path
// separators are normalised away so callers can join safely.
//
// Under `go test` a test never reads nor writes the operator's: an
// ITERION_HOME inherited from the operator is ignored and the fallback is the
// test process's own home (testIterionHome) — unless the test asked for the
// production resolution (ResolveIterionHomeAsInProductionForTests), which
// then resolves only a home dir the test owns.
func IterionHome() (string, error) {
	return resolveIterionHome(testing.Testing(), liveEnvironment)
}

// InheritedIterionHome is IterionHome read from the environment the process
// INHERITED (internal/envtrust): an ITERION_HOME or home dir a project `.env`
// planted names no home here. The readers that decide what the operator
// vouches for — the cross-store read gate, the global runs view — resolve it,
// so a repository's `.env` cannot re-root them.
func InheritedIterionHome() (string, error) {
	return resolveIterionHome(testing.Testing(), inheritedEnvironment)
}

// environment is where the iterion home is read from.
type environment struct {
	name   string
	getenv func(string) string
}

var (
	liveEnvironment      = environment{name: "the environment", getenv: os.Getenv}
	inheritedEnvironment = environment{name: "the environment the process inherited (a value a project .env set does not count)", getenv: envtrust.Inherited}
)

// resolveIterionHome is IterionHome read from env, with the test binary's
// resolution switched by underTest, so a test drives the production one too.
func resolveIterionHome(underTest bool, env environment) (string, error) {
	if !underTest {
		return iterionHome(env.getenv("ITERION_HOME"), env)
	}
	explicit := env.getenv("ITERION_HOME")
	if inheritedFromTheOperator(explicit) {
		explicit = ""
	}
	if !testHomeOff.Load() {
		if dir := trimSeparators(explicit); dir != "" {
			return absOrAsGiven(dir), nil
		}
		return testIterionHome(), nil
	}
	if !homeOwnedByTheTest() {
		return "", fmt.Errorf("store: %s=%q is not a directory the test owns: refusing to resolve the iterion home as in production", homeEnvName(), os.Getenv(homeEnvName()))
	}
	return iterionHome(explicit, env)
}

// iterionHome is the production resolution: explicit (an $ITERION_HOME
// value), else ~/.iterion — the home dir read from env, as os.UserHomeDir
// reads it — else an error.
func iterionHome(explicit string, env environment) (string, error) {
	if dir := trimSeparators(explicit); dir != "" {
		return absOrAsGiven(dir), nil
	}
	home := env.getenv(homeEnvName())
	if home == "" {
		return "", fmt.Errorf("store: resolve the iterion home: %s is not set in %s", homeEnvName(), env.name)
	}
	return filepath.Join(absOrAsGiven(home), StoreDirName), nil
}

func trimSeparators(p string) string {
	return strings.TrimRight(p, string(filepath.Separator))
}

// processIterionHome is $ITERION_HOME as the process started, before a test
// main or a test set its own.
var processIterionHome = os.Getenv("ITERION_HOME")

// inheritedFromTheOperator reports whether v is the ITERION_HOME the test
// process inherited from the operator — real data, whatever its spelling —
// rather than one a test main or a test set. A test home (hometest.Isolate's,
// handed to a re-executed test binary) stays the test's own.
func inheritedFromTheOperator(v string) bool {
	if trimSeparators(v) == "" || trimSeparators(processIterionHome) == "" {
		return false
	}
	c := filepath.Clean(v)
	return c == filepath.Clean(processIterionHome) && !strings.HasPrefix(filepath.Base(c), testHomePrefix)
}

// absOrAsGiven makes p absolute; when the working directory cannot be
// resolved it keeps p as given rather than replace an explicit value.
func absOrAsGiven(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// processTempDir is the system temp dir as the process started — absolute —
// before a test can point TMPDIR at its own t.TempDir(), which is removed
// when that test ends.
var processTempDir = absOrAsGiven(os.TempDir())

// testHomePrefix names the iterion home of a test process.
const testHomePrefix = "iterion-test-home-"

// testIterionHome is the iterion home of a test process that runs without
// ITERION_HOME: a path unique to the process, drawn once at random under the
// temp dir it started with. Resolving it creates nothing — a writer does, as
// for any home — so a package whose tests merely resolve paths leaves no
// trace. A package whose tests reach the iterion home wraps its TestMain in
// internal/hometest.Isolate, which sets ITERION_HOME to a directory it
// removes at exit and releases this one; this is the net under every other
// package.
var testIterionHome = sync.OnceValue(func() string {
	return filepath.Join(processTempDir, testHomePrefix+rand.Text())
})

// TestProcessDir is a directory owned by the running test process — the path
// IterionHome falls back to under `go test` — for other per-user state a test
// must keep off the operator's machine, such as the desktop project registry.
// It reports false outside a test binary. Nothing creates it but a writer,
// and internal/hometest.Isolate removes it at exit.
func TestProcessDir() (string, bool) {
	return testProcessDir(testing.Testing())
}

// testProcessDir is TestProcessDir with the test binary switched by
// underTest, so a test drives the production answer too.
func testProcessDir(underTest bool) (string, bool) {
	if !underTest {
		return "", false
	}
	return testIterionHome(), true
}

// ReleaseTestIterionHomeForTests removes the iterion home of this test
// process if a writer created it. Test mains call it at exit
// (internal/hometest.Isolate). Production never does.
func ReleaseTestIterionHomeForTests() error {
	if err := os.RemoveAll(testIterionHome()); err != nil {
		return fmt.Errorf("store: remove the test iterion home: %w", err)
	}
	return nil
}

// testHomeOff makes IterionHome resolve as in production inside a test
// binary; see ResolveIterionHomeAsInProductionForTests.
var testHomeOff atomic.Bool

// ResolveIterionHomeAsInProductionForTests makes IterionHome and
// InheritedIterionDataDir, for the rest of tb, resolve as a production binary
// does — $ITERION_HOME, else ~/.iterion, else an error — which is what a test
// of the no-home refusal needs. The home
// dir variable (HOME; USERPROFILE on Windows) must be "" or a directory the
// test owns under a temp root, when this is called and at every resolution
// after: a home that turns foreign makes IterionHome fail and
// InheritedIterionDataDir name no home, so the operator's own home stays out
// of reach. It changes process-wide state: never from a parallel test.
func ResolveIterionHomeAsInProductionForTests(tb testing.TB) {
	tb.Helper()
	if !homeOwnedByTheTest() {
		tb.Fatalf("store: point %s at a directory the test owns under %v (or at \"\") before resolving the iterion home as in production; %s=%q", homeEnvName(), testTempRoots(), homeEnvName(), os.Getenv(homeEnvName()))
	}
	testHomeOff.Store(true)
	tb.Cleanup(func() { testHomeOff.Store(false) })
}

// homeOwnedByTheTest reports whether the home dir os.UserHomeDir would read
// is unset or lies strictly inside one of the test's temp roots.
func homeOwnedByTheTest() bool {
	home := os.Getenv(homeEnvName())
	return home == "" || ownedByTheTest(home)
}

// testTempRoots are the directories a test's own files live under: the temp
// dir as the process started — a test main's or a test's own TMPDIR lies
// inside it, and t.TempDir creates a test's root before any switch — and
// GOTMPDIR, which t.TempDir honours, when absolute: a relative one names
// wherever the working directory happens to be at the check.
func testTempRoots() []string {
	roots := []string{processTempDir}
	if g := os.Getenv("GOTMPDIR"); filepath.IsAbs(g) {
		roots = append(roots, g)
	}
	return roots
}

// ownedByTheTest reports whether home lies strictly inside one of the test's
// temp roots.
func ownedByTheTest(home string) bool {
	for _, root := range testTempRoots() {
		if strictlyInside(home, root) {
			return true
		}
	}
	return false
}

// strictlyInside reports whether p lies inside root, root itself excluded.
func strictlyInside(p, root string) bool {
	rel, err := filepath.Rel(absOrAsGiven(root), absOrAsGiven(p))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// EncodeWorkDirKey produces a deterministic, filesystem-safe key
// from an absolute workdir path. Path separators are replaced with
// "-"; on Windows the drive ":" is also collapsed; the result always
// starts with "-" so a relative-looking input still produces a
// distinct key (and so the leading separator on Unix is preserved
// rather than dropped silently).
//
//	Unix:    "/home/jo/lab/devthefuture/modjo"
//	      -> "-home-jo-lab-devthefuture-modjo"
//
//	Windows: "C:\\foo\\bar"
//	      -> "-C-foo-bar"
//
// Different absolute paths therefore yield different keys, including
// when one project is a clone of another at a different location.
func EncodeWorkDirKey(absPath string) string {
	p := filepath.ToSlash(absPath)
	// Replace both separator flavours regardless of the runtime OS
	// so the key for a given path is the same whether iterion sees
	// it via a Windows-style or Unix-style spelling. ToSlash already
	// handles the host-OS case; the explicit backslash sweep covers
	// Unix hosts that happen to be passed a Windows path string
	// (rare, but cheap to be defensive about — keys are forever).
	p = strings.ReplaceAll(p, `\`, "-")
	p = strings.ReplaceAll(p, ":", "-")
	p = strings.ReplaceAll(p, "/", "-")
	if !strings.HasPrefix(p, "-") {
		p = "-" + p
	}
	return p
}
