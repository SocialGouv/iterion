package hometest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run sees a home Isolate created — never the operator's ITERION_HOME —
// its exit code comes back unchanged, and neither that home nor the one
// pkg/store makes for a test that clears ITERION_HOME survives Isolate.
func TestIsolate_TheRunGetsItsOwnHomeAndNothingSurvives(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	t.Setenv("ITERION_HOME", "/operator/real/iterion-home")

	var isolated, net string
	code := Isolate(func() int {
		isolated = os.Getenv("ITERION_HOME")
		// Nothing is written unless the home is one Isolate made under this
		// test's own TMPDIR: an empty or foreign value must not turn the
		// writes below into writes into the package directory or elsewhere.
		if !filepath.IsAbs(isolated) || filepath.Dir(isolated) != parent {
			t.Errorf("the run saw ITERION_HOME=%q, want a directory Isolate created under %s", isolated, parent)
			return 1
		}
		if err := os.MkdirAll(filepath.Join(isolated, "projects", "-tmp-TestX1-001", "runs"), 0o700); err != nil {
			t.Errorf("write into the isolated home: %v", err)
		}
		// A test that clears ITERION_HOME lands on pkg/store's net.
		if err := os.Setenv("ITERION_HOME", ""); err != nil {
			t.Errorf("clear ITERION_HOME: %v", err)
		}
		var err error
		if net, err = store.IterionHome(); err != nil {
			t.Errorf("IterionHome: %v", err)
			return 7
		}
		// Same guard for pkg/store's net before a store-like write lands in it.
		if !filepath.IsAbs(net) || !strings.HasPrefix(filepath.Base(net), homePrefix) {
			t.Errorf("pkg/store resolved %q, want its own %s* home", net, homePrefix)
			return 7
		}
		if err := os.MkdirAll(filepath.Join(net, "projects", "-tmp-TestX2-001", "runs"), 0o700); err != nil {
			t.Errorf("write into pkg/store's test home: %v", err)
		}
		return 7
	})

	if code != 7 {
		t.Fatalf("Isolate returned %d, want the run's 7", code)
	}
	if isolated == "/operator/real/iterion-home" || filepath.Dir(isolated) != parent {
		t.Fatalf("the run saw ITERION_HOME=%q, want a directory Isolate created under %s", isolated, parent)
	}
	for _, home := range []string{isolated, net} {
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("test iterion home %s survived Isolate (stat err=%v)", home, err)
		}
	}
}

// A planted-names marker the test binary inherited — an iterion process above
// the run planted ITERION_HOME — is dropped: the home Isolate sets reads as
// the operator's choice, not as one a project `.env` planted.
func TestIsolate_TheRunInheritsNoPlantedMarker(t *testing.T) {
	t.Setenv("ITERION_HOME", "") // Isolate sets it process-wide: restored after the test
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(envtrust.EnvPlantedNames, "ITERION_HOME")
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)

	var marker string
	var planted bool
	if code := Isolate(func() int {
		marker = os.Getenv(envtrust.EnvPlantedNames)
		planted = envtrust.Planted("ITERION_HOME")
		return 0
	}); code != 0 {
		t.Fatalf("Isolate returned %d, want the run's 0", code)
	}
	if marker != "" || planted {
		t.Fatalf("the run inherited the planted-names marker %q (ITERION_HOME planted: %v)", marker, planted)
	}
}

// A clean run whose home cannot be removed fails the suite: a leftover is
// the leak Isolate exists to prevent.
func TestIsolate_AHomeLeftBehindFailsACleanRun(t *testing.T) {
	t.Setenv("ITERION_HOME", "") // Isolate sets it process-wide: restored after the test
	if os.Geteuid() == 0 {
		t.Skip("root removes entries of a read-only directory: the leftover cannot be staged")
	}
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	var home string
	code := Isolate(func() int {
		home = os.Getenv("ITERION_HOME")
		if !filepath.IsAbs(home) || filepath.Dir(home) != parent {
			t.Errorf("the run saw ITERION_HOME=%q, want a directory Isolate created under %s", home, parent)
			return 0
		}
		// A read-only subdirectory with an entry: RemoveAll cannot empty it.
		locked := filepath.Join(home, "locked")
		if err := os.MkdirAll(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		return 0
	})
	if code == 0 {
		t.Fatalf("Isolate returned 0 although %s could not be removed", home)
	}
}

// removeHome only removes what isTestHome accepts: a home isolateHome
// itself created for that prefix. The predicate's domain is exercised here on
// paths alone; TestRemoveHome_RefusesAndKeepsADirItDidNotCreate drives the
// removal itself, on directories the test owns.
func TestIsTestHome_AcceptsOnlyAHomeIsolateCreates(t *testing.T) {
	parent := filepath.Join(string(filepath.Separator), "tmp", "parent")
	for _, tc := range []struct {
		dir    string
		prefix string
		want   bool
	}{
		{filepath.Join(parent, homePrefix+"123"), homePrefix, true},
		{filepath.Join(parent, userHomePrefix+"123"), userHomePrefix, true},
		{string(filepath.Separator), homePrefix, false},
		{parent, homePrefix, false},
		{filepath.Join(parent, "not-a-test-home"), homePrefix, false},
		{filepath.Join(parent, homePrefix+"123", "nested"), homePrefix, false},
		{filepath.Join(string(filepath.Separator), "tmp", "other", homePrefix+"123"), homePrefix, false},
		{filepath.Join(parent, "..", homePrefix+"123"), homePrefix, false},
		// A home of one kind is not accepted for the other's prefix.
		{filepath.Join(parent, userHomePrefix+"123"), homePrefix, false},
		{filepath.Join(parent, homePrefix+"123"), userHomePrefix, false},
	} {
		if got := isTestHome(parent, tc.dir, tc.prefix); got != tc.want {
			t.Errorf("isTestHome(%s, %s, %s) = %v, want %v", parent, tc.dir, tc.prefix, got, tc.want)
		}
	}
}

// removeHome refuses, and leaves in place, a directory isolateHome did not
// create for the prefix it is given. Every directory here is one the test made
// under its own t.TempDir(), so a broken guard removes nothing else.
func TestRemoveHome_RefusesAndKeepsADirItDidNotCreate(t *testing.T) {
	parent := t.TempDir()
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{"not-a-test-home", userHomePrefix},
		{homePrefix + "123", userHomePrefix},
		{userHomePrefix + "123", homePrefix},
	} {
		dir := filepath.Join(parent, tc.name)
		kept := filepath.Join(dir, "kept")
		if err := os.MkdirAll(kept, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := removeHome(parent, dir, tc.prefix, "test home"); err == nil {
			t.Errorf("removeHome(%s, prefix %s) = nil, want a refusal", dir, tc.prefix)
		}
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("removeHome removed %s, which it did not create for prefix %s: %v", dir, tc.prefix, err)
		}
	}
}

// The run sees an empty HOME IsolateUserHome created — never the operator's,
// whose login profile a `sh -lc` spawn would source — its exit code comes back
// unchanged, and that home, with what the run wrote into it, does not survive.
func TestIsolateUserHome_TheRunGetsAnEmptyHomeAndNothingSurvives(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	t.Setenv("HOME", "/operator/real/home") // IsolateUserHome sets HOME process-wide: restored after the test

	var home string
	code := IsolateUserHome(func() int {
		home = os.Getenv("HOME")
		// Nothing is written unless the home is one IsolateUserHome made under
		// this test's own TMPDIR.
		if !filepath.IsAbs(home) || filepath.Dir(home) != parent {
			t.Errorf("the run saw HOME=%q, want a directory IsolateUserHome created under %s", home, parent)
			return 1
		}
		if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
			t.Errorf("the run's HOME %s is not an empty directory: entries %v, err %v", home, entries, err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".cache", "iterion"), 0o700); err != nil {
			t.Errorf("write into the isolated home: %v", err)
		}
		return 7
	})

	if code != 7 {
		t.Fatalf("IsolateUserHome returned %d, want the run's 7", code)
	}
	if home == "/operator/real/home" || filepath.Dir(home) != parent || !strings.HasPrefix(filepath.Base(home), userHomePrefix) {
		t.Fatalf("the run saw HOME=%q, want a %s* directory IsolateUserHome created under %s", home, userHomePrefix, parent)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("test user home %s survived IsolateUserHome (stat err=%v)", home, err)
	}
}

// A clean run whose user home cannot be removed fails the suite, as for
// Isolate's iterion home.
func TestIsolateUserHome_AHomeLeftBehindFailsACleanRun(t *testing.T) {
	t.Setenv("HOME", "") // IsolateUserHome sets HOME process-wide: restored after the test
	if os.Geteuid() == 0 {
		t.Skip("root removes entries of a read-only directory: the leftover cannot be staged")
	}
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	var home string
	code := IsolateUserHome(func() int {
		home = os.Getenv("HOME")
		if !filepath.IsAbs(home) || filepath.Dir(home) != parent {
			t.Errorf("the run saw HOME=%q, want a directory IsolateUserHome created under %s", home, parent)
			return 0
		}
		// A read-only subdirectory with an entry: RemoveAll cannot empty it.
		locked := filepath.Join(home, "locked")
		if err := os.MkdirAll(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		return 0
	})
	if code == 0 {
		t.Fatalf("IsolateUserHome returned 0 although %s could not be removed", home)
	}
}

// A relative TMPDIR still gives the run an absolute home: a relative
// ITERION_HOME would resolve against whatever directory each reader and
// writer runs from.
func TestIsolate_ARelativeTempDirStillGivesAnAbsoluteHome(t *testing.T) {
	t.Setenv("ITERION_HOME", "") // Isolate sets it process-wide: restored after the test
	t.Chdir(t.TempDir())
	if err := os.Mkdir("reltmp", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", "reltmp")
	var home string
	code := Isolate(func() int {
		home = os.Getenv("ITERION_HOME")
		return 0
	})
	if code != 0 || !filepath.IsAbs(home) {
		t.Fatalf("Isolate gave the run ITERION_HOME=%q (code %d), want an absolute directory", home, code)
	}
}

// A clean run whose pkg/store test home cannot be removed fails the suite
// too: Isolate answers for the release it performs, not only for its own
// home.
func TestIsolate_AStoreTestHomeLeftBehindFailsACleanRun(t *testing.T) {
	t.Setenv("ITERION_HOME", "") // Isolate sets it process-wide: restored after the test
	if os.Geteuid() == 0 {
		t.Skip("root removes entries of a read-only directory: the leftover cannot be staged")
	}
	t.Setenv("TMPDIR", t.TempDir())
	var locked string
	code := Isolate(func() int {
		if err := os.Setenv("ITERION_HOME", ""); err != nil {
			t.Errorf("clear ITERION_HOME: %v", err)
			return 0
		}
		net, err := store.IterionHome()
		if err != nil || !filepath.IsAbs(net) || !strings.HasPrefix(filepath.Base(net), homePrefix) {
			t.Errorf("pkg/store resolved %q, %v; want its own %s* home", net, err, homePrefix)
			return 0
		}
		locked = filepath.Join(net, "locked")
		if err := os.MkdirAll(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(locked, 0o700)
			if err := store.ReleaseTestIterionHomeForTests(); err != nil {
				t.Errorf("release the staged store home: %v", err)
			}
		})
		return 0
	})
	if code == 0 {
		t.Fatalf("Isolate returned 0 although the store's test home %s could not be removed", filepath.Dir(locked))
	}
}

// fatalRecorder stands in for a test so RequireNoOperatorProfile's verdict can
// be read: Fatalf records the message and ends the goroutine, as t.Fatalf does.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// requireNoOperatorProfile runs RequireNoOperatorProfile on home and returns
// the failure it reported, empty when it passed.
func requireNoOperatorProfile(t *testing.T, home string) string {
	r := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RequireNoOperatorProfile(r, home)
	}()
	<-done
	return r.fatal
}

// Each check of RequireNoOperatorProfile refuses its own case alone — a HOME
// that is not absolute (a shell whose environment lacks HOME), the inherited
// one, a home holding a login profile — and an empty home of the binary's own
// passes.
func TestRequireNoOperatorProfile_RefusesEachWayAProfileGetsSourced(t *testing.T) {
	inherited := t.TempDir()
	saved := inheritedUserHome
	inheritedUserHome = inherited
	t.Cleanup(func() { inheritedUserHome = saved })

	for _, tc := range []struct {
		name, home, want string
	}{
		{"empty", "", "want the absolute directory"},
		{"relative", "rel/home", "want the absolute directory"},
		{"inherited", inherited, "ran under the inherited HOME"},
		{"inherited, trailing slash", inherited + "/", "ran under the inherited HOME"},
	} {
		if got := requireNoOperatorProfile(t, tc.home); !strings.Contains(got, tc.want) {
			t.Errorf("%s: RequireNoOperatorProfile(%q) reported %q, want a failure containing %q", tc.name, tc.home, got, tc.want)
		}
	}
	for _, rc := range []string{".profile", ".bash_profile", ".bash_login"} {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, rc), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, want := requireNoOperatorProfile(t, home), "holds a "+rc; !strings.Contains(got, want) {
			t.Errorf("a home holding %s: RequireNoOperatorProfile reported %q, want a failure containing %q", rc, got, want)
		}
	}
	if got := requireNoOperatorProfile(t, t.TempDir()); got != "" {
		t.Errorf("an empty home of the binary's own: RequireNoOperatorProfile reported %q, want it to pass", got)
	}
}
