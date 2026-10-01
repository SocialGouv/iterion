package store

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// Under `go test`, without ITERION_HOME, the iterion home is a path of this
// test process under the temp dir it started with — never the operator's
// ~/.iterion — and the writers' path lands in it: tests that set nothing
// wrote 130 966 run stores into ~/.iterion/projects (#2016).
func TestIterionHome_UnderGoTestIsTheProcessOwn(t *testing.T) {
	t.Setenv("ITERION_HOME", "")
	got, err := IterionHome()
	if err != nil {
		t.Fatalf("IterionHome: %v", err)
	}
	if operator, herr := os.UserHomeDir(); herr == nil && got == filepath.Join(operator, StoreDirName) {
		t.Fatalf("under go test the iterion home is the operator's %s", got)
	}
	if filepath.Dir(got) != processTempDir || !strings.HasPrefix(filepath.Base(got), testHomePrefix) {
		t.Fatalf("iterion home %q is not a %s* path of this process under %s", got, testHomePrefix, processTempDir)
	}
	if again, _ := IterionHome(); again != got {
		t.Fatalf("iterion home is not stable within the process: %q then %q", got, again)
	}
	if data := GlobalIterionDataDir(); data != got {
		t.Fatalf("GlobalIterionDataDir = %q, want the process's iterion home %q", data, got)
	}
	start := t.TempDir()
	abs, err := filepath.Abs(start)
	if err != nil {
		t.Fatal(err)
	}
	if sd, want := ResolveStoreDir(start, ""), filepath.Join(got, "projects", EncodeWorkDirKey(abs)); sd != want {
		t.Fatalf("ResolveStoreDir = %q, want the per-project slot under the process's home %q", sd, want)
	}
}

// An ITERION_HOME the test process inherited from the operator names real
// data: under `go test` it is ignored — unless it is a test home, as a
// re-executed test binary inherits hometest's — while one a test sets is
// honoured.
func TestIterionHome_IgnoresAnITERION_HOMEInheritedFromTheOperator(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	prev := processIterionHome
	t.Cleanup(func() { processIterionHome = prev })

	processIterionHome = "/operator/real/iterion-home"
	t.Setenv("ITERION_HOME", processIterionHome)
	if got, err := IterionHome(); err != nil || got != testIterionHome() {
		t.Fatalf("with the operator's inherited ITERION_HOME, IterionHome() = %q, %v; want the process's own %q", got, err, testIterionHome())
	}
	t.Setenv("ITERION_HOME", processIterionHome+string(filepath.Separator))
	if got, err := IterionHome(); err != nil || got != testIterionHome() {
		t.Fatalf("with the operator's inherited ITERION_HOME spelt with a trailing separator, IterionHome() = %q, %v; want the process's own %q", got, err, testIterionHome())
	}
	// The value the process inherited may carry the trailing separator
	// itself: it still names the operator's data, for both resolutions.
	processIterionHome = "/operator/real/iterion-home" + string(filepath.Separator)
	t.Setenv("ITERION_HOME", "/operator/real/iterion-home")
	if got, err := IterionHome(); err != nil || got != testIterionHome() {
		t.Fatalf("with the operator's ITERION_HOME inherited with a trailing separator, IterionHome() = %q, %v; want the process's own %q", got, err, testIterionHome())
	}
	if got := InheritedIterionDataDir(); got != testIterionHome() {
		t.Fatalf("with the operator's ITERION_HOME inherited with a trailing separator, InheritedIterionDataDir() = %q; want the process's own %q", got, testIterionHome())
	}

	inheritedTestHome := filepath.Join(t.TempDir(), testHomePrefix+"parent")
	processIterionHome = inheritedTestHome
	t.Setenv("ITERION_HOME", inheritedTestHome)
	if got, err := IterionHome(); err != nil || got != inheritedTestHome {
		t.Fatalf("with an inherited test home, IterionHome() = %q, %v; want it %q", got, err, inheritedTestHome)
	}

	set := t.TempDir()
	t.Setenv("ITERION_HOME", set)
	if got, err := IterionHome(); err != nil || got != set {
		t.Fatalf("with an ITERION_HOME the test set, IterionHome() = %q, %v; want it %q", got, err, set)
	}
}

// The production resolution: ~/.iterion made absolute — a relative HOME
// resolves against the working directory at resolution — and no home dir at
// all is an error rather than a guess: the readers refuse it, the writers
// fall back.
func TestIterionHome_ProductionIsDotIterionOrAnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, err := iterionHome("", liveEnvironment); err != nil || got != filepath.Join(home, StoreDirName) {
		t.Fatalf("iterionHome(\"\") = %q, %v; want %q", got, err, filepath.Join(home, StoreDirName))
	}

	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "relhome")
	if got, err := iterionHome("", liveEnvironment); err != nil || got != filepath.Join(cwd, "relhome", StoreDirName) {
		t.Fatalf("relative HOME: iterionHome(\"\") = %q, %v; want %q", got, err, filepath.Join(cwd, "relhome", StoreDirName))
	}

	t.Setenv("HOME", "")
	if got, err := iterionHome("", liveEnvironment); err == nil {
		t.Fatalf("no home dir resolves, yet iterionHome(\"\") = %q with no error", got)
	}
}

// A production binary takes none of the test process's branches: it honours
// the ITERION_HOME it inherited and names ~/.iterion where a test process
// names its own home, and it has no test process dir. Driven through the
// twins the exported functions hand testing.Testing(), so a twin whose gate is
// dropped or inverted fails here; that the exported ones hand it is
// TestABuiltBinaryResolvesTheOperatorsHome's to show.
func TestTheProductionResolutionTakesNoTestBranch(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	prev := processIterionHome
	t.Cleanup(func() { processIterionHome = prev })

	inherited := filepath.Join(t.TempDir(), "operator-home")
	processIterionHome = inherited
	t.Setenv("ITERION_HOME", inherited)
	t.Setenv(homeEnvName(), filepath.Join(t.TempDir(), "user"))
	if got, err := resolveIterionHome(false, liveEnvironment); err != nil || got != inherited {
		t.Fatalf("production, the ITERION_HOME it inherited: resolveIterionHome = %q, %v; want %q", got, err, inherited)
	}
	if got := inheritedIterionDataDir(false); got != inherited {
		t.Fatalf("production, the ITERION_HOME it inherited: inheritedIterionDataDir = %q; want %q", got, inherited)
	}

	user := filepath.Join(t.TempDir(), "user")
	t.Setenv("ITERION_HOME", "")
	t.Setenv(homeEnvName(), user)
	want := filepath.Join(user, StoreDirName)
	if got, err := resolveIterionHome(false, liveEnvironment); err != nil || got != want {
		t.Fatalf("production, no ITERION_HOME: resolveIterionHome = %q, %v; want %q", got, err, want)
	}
	if got := inheritedIterionDataDir(false); got != want {
		t.Fatalf("production, no ITERION_HOME: inheritedIterionDataDir = %q; want %q", got, want)
	}

	// From the environment the process inherited, an ITERION_HOME a project
	// `.env` planted names no home — the home dir tier decides — and a
	// planted home dir names none either.
	planted := filepath.Join(t.TempDir(), "planted")
	t.Setenv("ITERION_HOME", planted)
	envtrust.MarkPlanted("ITERION_HOME")
	if got, err := resolveIterionHome(false, liveEnvironment); err != nil || got != planted {
		t.Fatalf("production, a planted ITERION_HOME: the live resolution = %q, %v; want it %q", got, err, planted)
	}
	if got, err := resolveIterionHome(false, inheritedEnvironment); err != nil || got != want {
		t.Fatalf("production, a planted ITERION_HOME: the inherited resolution = %q, %v; want %q", got, err, want)
	}
	envtrust.MarkPlanted(homeEnvName())
	if got, err := resolveIterionHome(false, inheritedEnvironment); err == nil {
		t.Fatalf("production, a planted %s: the inherited resolution named %q; want no home", homeEnvName(), got)
	}

	// HOME=/ is a home — /.iterion — for both resolutions: the inherited one
	// joins the value rather than trimming it to nothing.
	t.Setenv("ITERION_HOME", "")
	t.Setenv(homeEnvName(), string(filepath.Separator))
	rootHome := filepath.Join(string(filepath.Separator), StoreDirName)
	envtrust.ResetForTest()
	t.Setenv(envtrust.EnvPlantedNames, "")
	if got, err := resolveIterionHome(false, liveEnvironment); err != nil || got != rootHome {
		t.Fatalf("production, HOME=/: resolveIterionHome = %q, %v; want %q", got, err, rootHome)
	}
	if got := inheritedIterionDataDir(false); got != rootHome {
		t.Fatalf("production, HOME=/: inheritedIterionDataDir = %q; want %q", got, rootHome)
	}

	// No home dir at all: the writers' <tmp>/iterion-data fallback is shared,
	// so the operator chose no home, whether TMPDIR was inherited or not.
	t.Setenv("ITERION_HOME", "")
	t.Setenv(homeEnvName(), "")
	for _, tmp := range []string{t.TempDir(), ""} {
		t.Setenv("TMPDIR", tmp)
		if got := inheritedIterionDataDir(false); got != "" {
			t.Fatalf("production, no home dir, TMPDIR=%q: inheritedIterionDataDir = %q; want none — the <tmp> fallback is shared", tmp, got)
		}
	}

	if dir, ok := testProcessDir(false); ok || dir != "" {
		t.Fatalf("production has no test process dir; testProcessDir = %q, %v", dir, ok)
	}
}

// Under `go test` as well, a home a project `.env` planted is no home for the
// readers: InheritedIterionHome falls back to the test process's own while
// IterionHome, the writers', follows the planted value.
func TestInheritedIterionHome_IgnoresAPlantedITERION_HOME(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")

	explicit := filepath.Join(t.TempDir(), "explicit")
	t.Setenv("ITERION_HOME", explicit)
	if got, err := InheritedIterionHome(); err != nil || got != explicit {
		t.Fatalf("an ITERION_HOME nothing planted: InheritedIterionHome() = %q, %v; want %q", got, err, explicit)
	}
	envtrust.MarkPlanted("ITERION_HOME")
	if got, err := InheritedIterionHome(); err != nil || got != testIterionHome() {
		t.Fatalf("a planted ITERION_HOME: InheritedIterionHome() = %q, %v; want the process's own %q", got, err, testIterionHome())
	}
	if got, err := IterionHome(); err != nil || got != explicit {
		t.Fatalf("a planted ITERION_HOME: IterionHome() = %q, %v; want the planted %q the writers use", got, err, explicit)
	}
}

// A binary `go build` made takes none of the test process's branches either:
// the exported functions hand testing.Testing() to the twins above, and only
// a production build shows that they do — with true handed instead, every
// test stays green while every operator's runs, plugin authority and desktop
// registry go to a temp dir. testdata/homeprobe prints the resolution; it runs
// the way an operator runs iterion, with the ITERION_HOME its shell exported
// and without one.
func TestABuiltBinaryResolvesTheOperatorsHome(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "homeprobe")
	build := exec.Command("go", "build", "-mod=vendor", "-o", bin, "./testdata/homeprobe")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./testdata/homeprobe: %v\n%s", err, out)
	}

	root := t.TempDir()
	explicit := filepath.Join(root, "explicit")
	user := filepath.Join(root, "user")
	dotIterion := filepath.Join(user, StoreDirName)
	for _, tc := range []struct {
		name string
		env  []string
		// want is the home the process writes under; operators is the one
		// the operator chose, which a planted ITERION_HOME does not move.
		want, operators string
	}{
		{"the ITERION_HOME it inherited", []string{"ITERION_HOME=" + explicit, homeEnvName() + "=" + user}, explicit, explicit},
		{"no ITERION_HOME", []string{homeEnvName() + "=" + user}, dotIterion, dotIterion},
		{"an ITERION_HOME a project .env planted", []string{"ITERION_HOME=" + explicit, homeEnvName() + "=" + user, envtrust.EnvPlantedNames + "=ITERION_HOME"}, explicit, dotIterion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin)
			cmd.Env = append([]string{"TMPDIR=" + t.TempDir()}, tc.env...)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("homeprobe: %v\n%s", err, out)
			}
			var got struct {
				IterionHome      string `json:"iterion_home"`
				IterionHomeError string `json:"iterion_home_error"`
				Global           string `json:"global"`
				Inherited        string `json:"inherited"`
				InheritedHome    string `json:"inherited_home"`
				InheritedHomeErr string `json:"inherited_home_error"`
				TestProcessDir   string `json:"test_process_dir"`
				UnderTest        bool   `json:"under_test"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("homeprobe output %q: %v", out, err)
			}
			if got.IterionHome != tc.want || got.IterionHomeError != "" {
				t.Errorf("IterionHome = %q, %q; want %q", got.IterionHome, got.IterionHomeError, tc.want)
			}
			if got.Global != tc.want {
				t.Errorf("GlobalIterionDataDir = %q; want %q", got.Global, tc.want)
			}
			if got.Inherited != tc.operators {
				t.Errorf("InheritedIterionDataDir = %q; want %q", got.Inherited, tc.operators)
			}
			if got.InheritedHome != tc.operators || got.InheritedHomeErr != "" {
				t.Errorf("InheritedIterionHome = %q, %q; want %q", got.InheritedHome, got.InheritedHomeErr, tc.operators)
			}
			if got.UnderTest || got.TestProcessDir != "" {
				t.Errorf("TestProcessDir = %q, %v; want none outside a test binary", got.TestProcessDir, got.UnderTest)
			}
		})
	}
}

// An explicit ITERION_HOME wins inside and outside a test binary, its
// trailing separator trimmed, and a relative value is made absolute: a
// reader comparing absolute store paths with a relative root refused every
// store the writers put under it.
func TestIterionHome_ExplicitWinsAbsolute(t *testing.T) {
	dir := t.TempDir()
	if got, err := iterionHome(dir+string(filepath.Separator), liveEnvironment); err != nil || got != dir {
		t.Fatalf("iterionHome(%q) = %q, %v; want %q", dir+string(filepath.Separator), got, err, dir)
	}
	t.Setenv("ITERION_HOME", dir+string(filepath.Separator))
	if got, err := IterionHome(); err != nil || got != dir {
		t.Fatalf("IterionHome() = %q, %v; want %q", got, err, dir)
	}

	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_HOME", "rel")
	if got, err := IterionHome(); err != nil || got != filepath.Join(cwd, "rel") {
		t.Fatalf("relative ITERION_HOME: IterionHome() = %q, %v; want %q", got, err, filepath.Join(cwd, "rel"))
	}
	if got, err := iterionHome("rel", liveEnvironment); err != nil || got != filepath.Join(cwd, "rel") {
		t.Fatalf("relative ITERION_HOME, production resolution: iterionHome(\"rel\") = %q, %v; want %q", got, err, filepath.Join(cwd, "rel"))
	}
}

// Releasing removes the test process's home once a writer created it; the
// path stays the process's own, absent until the next writer.
func TestReleaseTestIterionHomeForTests_RemovesWhatAWriterCreated(t *testing.T) {
	t.Setenv("ITERION_HOME", "")
	home, err := IterionHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "projects", "-tmp-TestW1-001", "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseTestIterionHomeForTests(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("released home %s still exists (lstat err=%v)", home, err)
	}
	if again, _ := IterionHome(); again != home {
		t.Fatalf("after release the process resolves %q, want its own %q", again, home)
	}
}

// The production-resolution hook: inside it IterionHome is HOME/.iterion, or
// an error without a home; a HOME that turns foreign after the call is an
// error at resolution, never the operator's ~/.iterion; once the (sub)test
// ends the process's own home is back.
func TestResolveIterionHomeAsInProductionForTests(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("ITERION_HOME", "")
	net, err := IterionHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("home owned by the test", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		ResolveIterionHomeAsInProductionForTests(t)
		if got, err := IterionHome(); err != nil || got != filepath.Join(home, StoreDirName) {
			t.Fatalf("IterionHome() = %q, %v; want %q", got, err, filepath.Join(home, StoreDirName))
		}
	})
	t.Run("no home", func(t *testing.T) {
		t.Setenv("HOME", "")
		ResolveIterionHomeAsInProductionForTests(t)
		if got, err := IterionHome(); err == nil {
			t.Fatalf("no home dir resolves, yet IterionHome() = %q with no error", got)
		}
	})
	t.Run("home turning foreign after the call", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		ResolveIterionHomeAsInProductionForTests(t)
		t.Setenv("HOME", "/nonexistent-operator/home")
		if got, err := IterionHome(); err == nil || !strings.Contains(err.Error(), "not a directory the test owns") {
			t.Fatalf("a foreign HOME resolved to %q, %v; want the refusal", got, err)
		}
		if got := InheritedIterionDataDir(); got != "" {
			t.Fatalf("a foreign HOME named %q as the operator's iterion home; want none", got)
		}
	})
	t.Run("the operator's inherited ITERION_HOME stays ignored", func(t *testing.T) {
		prev := processIterionHome
		t.Cleanup(func() { processIterionHome = prev })
		processIterionHome = "/operator/real/iterion-home"
		t.Setenv("ITERION_HOME", processIterionHome)
		home := t.TempDir()
		t.Setenv("HOME", home)
		ResolveIterionHomeAsInProductionForTests(t)
		if got, err := IterionHome(); err != nil || got != filepath.Join(home, StoreDirName) {
			t.Fatalf("with the operator's inherited ITERION_HOME, IterionHome() = %q, %v; want the test's %q", got, err, filepath.Join(home, StoreDirName))
		}
	})
	if got, err := IterionHome(); err != nil || got != net {
		t.Fatalf("after the hook's tests, IterionHome() = %q, %v; want the process's own %q back", got, err, net)
	}
}

// Ownership is "strictly inside a temp root": the predicate is exercised
// alone — a real HOME is never handed to the hook.
func TestStrictlyInside(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "claude-1000")
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{filepath.Join(root, "TestX1", "001"), true},
		{root, false},
		{filepath.Join(root, ".."), false},
		{filepath.Join(string(filepath.Separator), "home", "jo"), false},
		{filepath.Join(root+"-other", "x"), false},
		{filepath.Join(root, "..", "claude-1000-other", "x"), false},
	} {
		if got := strictlyInside(tc.p, root); got != tc.want {
			t.Errorf("strictlyInside(%s, %s) = %v, want %v", tc.p, root, got, tc.want)
		}
	}
}

// t.TempDir honours GOTMPDIR: a HOME created there is the test's own. The
// root lies outside every other temp root, so only GOTMPDIR can own it — the
// check is a pure path computation, nothing is created there. A relative
// GOTMPDIR is no root: it names wherever the working directory happens to be
// at the check, which can sit above a home the test does not own.
func TestOwnedByTheTest_HonoursGOTMPDIR(t *testing.T) {
	gotmp := filepath.Join(string(filepath.Separator), "nonexistent-gotmp-root")
	home := filepath.Join(gotmp, "TestX1", "001")
	if ownedByTheTest(home) {
		t.Fatalf("%s is owned by the test before GOTMPDIR names its root — the case no longer isolates GOTMPDIR", home)
	}
	t.Setenv("GOTMPDIR", gotmp)
	if !ownedByTheTest(home) {
		t.Fatalf("a dir under GOTMPDIR %s is not owned by the test", gotmp)
	}

	t.Chdir(string(filepath.Separator))
	t.Setenv("GOTMPDIR", strings.TrimPrefix(gotmp, string(filepath.Separator)))
	if ownedByTheTest(home) {
		t.Fatalf("the relative GOTMPDIR %q made %s the test's own", os.Getenv("GOTMPDIR"), home)
	}
}

// Environment of the re-executed child of
// TestIterionHome_TheFirstLookupOfAProcessCreatesNothing.
const (
	firstLookupChildEnv  = "ITERION_STORE_FIRST_LOOKUP_CHILD"
	firstLookupParentEnv = "ITERION_STORE_FIRST_LOOKUP_PARENT"
	firstLookupDecoyEnv  = "ITERION_STORE_FIRST_LOOKUP_DECOY"
)

// A process's test home sits under the temp dir the process STARTED with,
// made absolute — never under a TMPDIR a test switches to later, which is
// removed with that test — its first resolution creates nothing (a net that
// materialised on lookup left an empty directory per package that merely
// resolves paths), and the ITERION_HOME the process was started with — the
// operator's, real data — is ignored. Only a fresh process shows all three,
// so the test re-executes itself, started with a relative TMPDIR and a decoy
// ITERION_HOME, and the child switches TMPDIR before resolving.
func TestIterionHome_TheFirstLookupOfAProcessCreatesNothing(t *testing.T) {
	if os.Getenv(firstLookupChildEnv) == "1" {
		decoy := os.Getenv(firstLookupDecoyEnv)
		t.Setenv("ITERION_HOME", decoy)
		t.Setenv("TMPDIR", t.TempDir())
		home, err := IterionHome()
		if err != nil {
			t.Fatalf("IterionHome: %v", err)
		}
		if home == decoy {
			t.Fatalf("the ITERION_HOME the process started with (%s) resolved as the iterion home", decoy)
		}
		if want := os.Getenv(firstLookupParentEnv); filepath.Dir(home) != want {
			t.Fatalf("the process's test home %s is not under the absolute temp dir it started with, %s", home, want)
		}
		if _, err := os.Lstat(home); !os.IsNotExist(err) {
			t.Fatalf("the first lookup of the process created %s (lstat err=%v)", home, err)
		}
		return
	}
	// The child's working directory is the physical path (os/exec leaves PWD
	// alone with an explicit Env): resolve symlinks — /var on macOS — first.
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, "reltmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(cwd, "operator-iterion-home")
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), firstLookupChildEnv+"=1", "TMPDIR=reltmp", "ITERION_HOME="+decoy,
		firstLookupDecoyEnv+"="+decoy, firstLookupParentEnv+"="+filepath.Join(cwd, "reltmp"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the re-executed lookup failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("the re-executed binary did not run %s:\n%s", t.Name(), out)
	}
}
