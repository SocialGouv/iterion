package hometest

import (
	"os"
	"path/filepath"
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

// removeHome only removes what isTestHome accepts: a home Isolate itself
// created. The predicate is exercised alone — a test never hands a foreign
// path to a real removal and trusts the guard to stop it.
func TestIsTestHome_AcceptsOnlyAHomeIsolateCreates(t *testing.T) {
	parent := filepath.Join(string(filepath.Separator), "tmp", "parent")
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{filepath.Join(parent, homePrefix+"123"), true},
		{string(filepath.Separator), false},
		{parent, false},
		{filepath.Join(parent, "not-a-test-home"), false},
		{filepath.Join(parent, homePrefix+"123", "nested"), false},
		{filepath.Join(string(filepath.Separator), "tmp", "other", homePrefix+"123"), false},
		{filepath.Join(parent, "..", homePrefix+"123"), false},
	} {
		if got := isTestHome(parent, tc.dir); got != tc.want {
			t.Errorf("isTestHome(%s, %s) = %v, want %v", parent, tc.dir, got, tc.want)
		}
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
