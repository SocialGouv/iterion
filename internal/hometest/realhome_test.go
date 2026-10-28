package hometest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// pointRealHomeAt aims the real-home guard at a directory the test owns —
// never at the operator's actual home.
func pointRealHomeAt(t *testing.T, home string) {
	t.Helper()
	saved := inheritedRealIterionHome
	inheritedRealIterionHome = home
	t.Cleanup(func() { inheritedRealIterionHome = saved })
}

// The entry filter names the exact test-key forms, never a blanket prefix:
// real projects live under /tmp too (#2011).
func TestRealHomeTestEntries_NamesOnlyTestKeys(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	legitTmp := "-tmp-my-real-project"
	legitOther := "-home-jo-lab-prod"
	leakDefault := "-tmp-TestFoo-001"
	leakInherited := store.EncodeWorkDirKey(os.TempDir()) + "-TestBar-002"
	for _, n := range []string{leakDefault, leakInherited, legitTmp, legitOther} {
		if err := os.MkdirAll(filepath.Join(projects, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pointRealHomeAt(t, home)
	got := realHomeTestEntries()
	if !got[leakDefault] || !got[leakInherited] {
		t.Fatalf("realHomeTestEntries missed a test key: got %v, want %s and %s", got, leakDefault, leakInherited)
	}
	if got[legitTmp] || got[legitOther] {
		t.Fatalf("realHomeTestEntries named a real project: got %v", got)
	}
}

// A test-key store that GAINED an entry over the run fails the suite.
func TestIsolate_FailsWhenTheRealHomeGainsATestStore(t *testing.T) {
	home := t.TempDir()
	pointRealHomeAt(t, home)
	code := Isolate(func() int {
		if err := os.MkdirAll(filepath.Join(inheritedRealIterionHome, "projects", "-tmp-TestLeak-001"), 0o700); err != nil {
			t.Errorf("stage the leak: %v", err)
		}
		return 0
	})
	if code == 0 {
		t.Fatal("Isolate returned 0: a test-key store gained in the real home went unreported")
	}
}

// The home is shared with the operator's live studio: a legitimate entry it
// gains over the run fails nothing.
func TestIsolate_ALegitimateEntryFailsNothing(t *testing.T) {
	home := t.TempDir()
	pointRealHomeAt(t, home)
	code := Isolate(func() int {
		if err := os.MkdirAll(filepath.Join(inheritedRealIterionHome, "projects", "-tmp-my-real-project"), 0o700); err != nil {
			t.Errorf("stage the legitimate entry: %v", err)
		}
		return 0
	})
	if code != 0 {
		t.Fatalf("Isolate returned %d: the guard failed a suite over a legitimate entry — the filter is blind", code)
	}
}

// Entries the home already held fail nothing: the delta is what the suite
// gained, not what it found.
func TestIsolate_PreexistingTestEntriesFailNothing(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "projects", "-tmp-TestOld-001"), 0o700); err != nil {
		t.Fatal(err)
	}
	pointRealHomeAt(t, home)
	code := Isolate(func() int { return 0 })
	if code != 0 {
		t.Fatalf("Isolate returned %d: the guard failed a suite over an entry the home already held", code)
	}
}
