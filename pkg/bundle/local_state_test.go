package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// catalogRelPath is the one path a catalogue regeneration replaces inside a
// materialized bundle.
var catalogRelPath = DirSkills + "/" + GeneratedCatalogFile

// shippedCatalogOwner writes a bundle as its PRODUCER ships one: the
// generated catalogue is a file of its own, so the pin in bots.lock covers
// those bytes. It returns the bundle root, the producer's catalogue bytes
// and the hash a pin would have been computed at.
func shippedCatalogOwner(t *testing.T) (dir string, producer []byte, pin string) {
	t.Helper()
	dir = t.TempDir()
	writeFragmentFixture(t, dir, MainBotFile, "workflow router {}\n")
	writeFragmentFixture(t, dir, ManifestFile, "name: router\nschema_version: 1\n")
	producer = []byte("the table the packer froze\n")
	writeFragmentFixture(t, dir, filepath.FromSlash(catalogRelPath), string(producer))
	pin, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, producer, pin
}

// keepAndRecord is what a regeneration does inside a pinned install: the
// producer's bytes go under KeptPathFor, the live table lands at the real
// path, and the record describes both.
func keepAndRecord(t *testing.T, dir string, producer, live []byte) {
	t.Helper()
	writeFragmentFixture(t, dir, filepath.FromSlash(KeptPathFor(catalogRelPath)), string(producer))
	writeFragmentFixture(t, dir, filepath.FromSlash(catalogRelPath), string(live))
	entry := LocalStateEntry{Path: catalogRelPath, SHA256: SHA256Bytes(live), KeptSHA256: SHA256Bytes(producer)}
	if err := WriteLocalStateOfKind(dir, LocalStateCatalog, []LocalStateEntry{entry}); err != nil {
		t.Fatal(err)
	}
}

// TestAReplacedCatalogIsHashedFromTheKeptBytes is the witness of ARB-109's
// answer: a router bundle vendored as a dependency has its generated
// catalogue rewritten with the CONSUMER workspace's routing table, and that
// write must not move the bundle's identity — the verdict every pin check
// reads. The producer ships this path, so excluding it would not do: the
// hash is computed from the bytes kept aside.
func TestAReplacedCatalogIsHashedFromTheKeptBytes(t *testing.T) {
	dir, producer, pin := shippedCatalogOwner(t)
	live := []byte("the table of the workspace that installed it\n")

	// Premise: an unrecorded rewrite IS drift, so the record is what holds
	// the hash still.
	writeFragmentFixture(t, dir, filepath.FromSlash(catalogRelPath), string(live))
	drifted, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if drifted == pin {
		t.Fatal("an unrecorded rewrite of the generated catalog left the hash alone; the record is not what holds it")
	}

	keepAndRecord(t, dir, producer, live)
	recorded, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if recorded != pin {
		t.Errorf("hash with a recorded catalog = %s, want the pinned %s: bots.lock reads the install as drifted", recorded, pin)
	}

	// And the run still reads the LIVE table: the file on disk is the
	// regenerated one, not the bytes the hash was taken from.
	onDisk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(catalogRelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(live) {
		t.Errorf("the catalog on disk = %q, want the live table %q", onDisk, live)
	}
}

// TestAKeptCopyThatNoLongerDescribesTheInstallIsDrift: the record is local
// state, not a trust boundary. Kept bytes that are not the producer's — or
// that are gone — must make the hash MOVE, which a sync repairs by
// re-materializing; a hard refusal would turn a deletion under .iterion into
// an unreadable bundle, and honouring them blindly would let any writer hide
// a replaced file from every pin check.
func TestAKeptCopyThatNoLongerDescribesTheInstallIsDrift(t *testing.T) {
	keptRel := filepath.FromSlash(KeptPathFor(catalogRelPath))
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, dir string)
	}{
		{name: "kept bytes edited", breakIt: func(t *testing.T, dir string) {
			writeFragmentFixture(t, dir, keptRel, "not what the producer shipped\n")
		}},
		{name: "kept copy removed", breakIt: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, keptRel)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, producer, pin := shippedCatalogOwner(t)
			live := []byte("the live table\n")
			// What the bundle hashes to when the record is ignored
			// altogether: the bytes that are actually at the path.
			writeFragmentFixture(t, dir, filepath.FromSlash(catalogRelPath), string(live))
			liveOnly, err := ContentHashDir(dir)
			if err != nil {
				t.Fatal(err)
			}

			keepAndRecord(t, dir, producer, live)
			tc.breakIt(t, dir)
			got, err := ContentHashDir(dir)
			if err != nil {
				t.Fatalf("ContentHashDir refused the bundle instead of reading it as drifted: %v", err)
			}
			if got == pin {
				t.Errorf("hash = %s, the pinned %s: a kept copy the record no longer describes was honoured", got, pin)
			}
			if got != liveOnly {
				t.Errorf("hash = %s, want %s — the bytes on disk: a kept copy the record no longer describes must count for nothing, not for itself", got, liveOnly)
			}
		})
	}
}

// TestTheRecordHoldsBothKindsAtOnce: one record, two kinds — a second
// registry would be a second truth about the same bundle. Each writer
// reconciles its OWN lines, so an install's fragment pass cannot erase the
// catalogue a regeneration recorded, nor the other way round.
func TestTheRecordHoldsBothKindsAtOnce(t *testing.T) {
	dir, producer, pin := shippedCatalogOwner(t)
	fragment := []byte("schema shared {}\n")
	writeFragmentFixture(t, dir, filepath.Join(DirFragments, "shared.bot"), string(fragment))

	keepAndRecord(t, dir, producer, []byte("the live table\n"))
	if err := WriteLocalStateOfKind(dir, LocalStateFragment, []LocalStateEntry{
		{Path: DirFragments + "/shared.bot", SHA256: SHA256Bytes(fragment)},
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadLocalState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("record = %+v, want the fragment AND the catalog", entries)
	}
	catalog, err := ReadLocalStateOfKind(dir, LocalStateCatalog)
	if err != nil || len(catalog) != 1 || catalog[0].Path != catalogRelPath || catalog[0].KeptSHA256 != SHA256Bytes(producer) {
		t.Fatalf("catalog entries = %+v, err=%v; want the one line with the producer's kept digest", catalog, err)
	}
	frags, err := ReadLocalStateOfKind(dir, LocalStateFragment)
	if err != nil || len(frags) != 1 || frags[0].Path != DirFragments+"/shared.bot" || frags[0].KeptSHA256 != "" {
		t.Fatalf("fragment entries = %+v, err=%v; want the one added path, which keeps nothing", frags, err)
	}

	// Both kinds honoured at once: the materialized fragment is out of the
	// hash and the replaced catalog is read from the kept bytes.
	got, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != pin {
		t.Errorf("hash with both kinds recorded = %s, want the pinned %s", got, pin)
	}

	// The fragment pass runs again and drops its dependency: the catalogue
	// line survives a reconciliation it does not belong to.
	if err := WriteLocalStateOfKind(dir, LocalStateFragment, nil); err != nil {
		t.Fatal(err)
	}
	after, err := ReadLocalStateOfKind(dir, LocalStateCatalog)
	if err != nil || len(after) != 1 {
		t.Fatalf("catalog entries after the fragment pass = %+v, err=%v; want the line it does not own left alone", after, err)
	}
}

// TestWriteLocalStateOfKindRefusesAnEntryOfAnotherKind: a writer that
// reconciles one kind must not be handed another's line, or the kind it
// claims to own stops being the kind it writes.
func TestWriteLocalStateOfKindRefusesAnEntryOfAnotherKind(t *testing.T) {
	dir := t.TempDir()
	err := WriteLocalStateOfKind(dir, LocalStateFragment, []LocalStateEntry{
		{Path: catalogRelPath, SHA256: SHA256Bytes([]byte("x"))},
	})
	if err == nil {
		t.Fatal("the fragment writer accepted a catalog line")
	}
	if !strings.Contains(err.Error(), "is catalog local state, not fragment") {
		t.Errorf("error = %q", err)
	}
}

// TestTheKeptCopyIsInvisibleToEveryWalkerOfABundle: the kept bytes sit under
// the control directory every content walker already skips, so no archive,
// content hash or snapshot carries them — and no new skip rule has to be
// kept in step with them.
func TestTheKeptCopyIsInvisibleToEveryWalkerOfABundle(t *testing.T) {
	if !IsPackSkipped(KeptPathFor(catalogRelPath)) {
		t.Errorf("IsPackSkipped(%q) = false: a kept copy would be packed and hashed as the bundle's own content", KeptPathFor(catalogRelPath))
	}
}

// TestARecordLineSaysWhatItMeans: the two arities are the two shapes of
// local state, and anything else is refused rather than guessed.
func TestARecordLineSaysWhatItMeans(t *testing.T) {
	dir := t.TempDir()
	digest := SHA256Bytes([]byte("a"))
	kept := SHA256Bytes([]byte("b"))
	writeFragmentFixture(t, dir, LocalStateFile, digest+" "+catalogRelPath+" "+kept+"\n")
	entries, err := ReadLocalState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SHA256 != digest || entries[0].Path != catalogRelPath || entries[0].KeptSHA256 != kept {
		t.Fatalf("entries = %+v, want the three-field line read back as it was written", entries)
	}

	writeFragmentFixture(t, dir, LocalStateFile, digest+" "+catalogRelPath+" "+kept+" extra\n")
	if _, err := ReadLocalState(dir); err == nil {
		t.Fatal("ReadLocalState accepted a line with a field nobody writes")
	}
}

// TestARecordedPathThatIsGoneIsDrift: a record describes files an install
// put there, and a file it no longer describes counts for nothing. Gone is
// the loudest case of that: the hash moves, and the pin check reads the
// install as drifted instead of meeting an error it cannot act on (a run
// resume would then fail on the hash instead of refusing with the identity
// it can name).
func TestARecordedPathThatIsGoneIsDrift(t *testing.T) {
	t.Run("a recorded catalog", func(t *testing.T) {
		dir, producer, pin := shippedCatalogOwner(t)
		keepAndRecord(t, dir, producer, []byte("the live table\n"))
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(catalogRelPath))); err != nil {
			t.Fatal(err)
		}
		got, err := ContentHashDir(dir)
		if err != nil {
			t.Fatalf("ContentHashDir refused a bundle missing a recorded file instead of reading it as drifted: %v", err)
		}
		if got == pin {
			t.Errorf("hash = %s, the pinned %s: a file that is gone was hashed from the kept bytes — a missing skill would read as pristine", got, pin)
		}
	})
	t.Run("a recorded fragment", func(t *testing.T) {
		dir, _, _ := shippedCatalogOwner(t)
		body := []byte("schema shared {}\n")
		writeFragmentFixture(t, dir, filepath.Join(DirFragments, "shared.bot"), string(body))
		if err := WriteLocalStateOfKind(dir, LocalStateFragment, []LocalStateEntry{
			{Path: DirFragments + "/shared.bot", SHA256: SHA256Bytes(body)},
		}); err != nil {
			t.Fatal(err)
		}
		withFragment, err := ContentHashDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, DirFragments, "shared.bot")); err != nil {
			t.Fatal(err)
		}
		got, err := ContentHashDir(dir)
		if err != nil {
			t.Fatalf("ContentHashDir refused a bundle missing a recorded fragment: %v", err)
		}
		if got != withFragment {
			t.Errorf("hash = %s, want %s: a materialized fragment is out of the hash whether it is there or not", got, withFragment)
		}
	})
}

// TestAPackedBundleCarriesTheKeptBytes: the hash and the archive come from
// one walk, so a materialized bundle repacked — the marketplace serving a
// tree, a dependency transaction staging one — ships the catalogue its
// PRODUCER published, and the archive reads back at the same identity. Two
// walkers, one answer.
func TestAPackedBundleCarriesTheKeptBytes(t *testing.T) {
	dir, producer, pin := shippedCatalogOwner(t)
	keepAndRecord(t, dir, producer, []byte("the live table\n"))

	out := filepath.Join(t.TempDir(), "router.botz")
	res, err := PackDir(dir, out)
	if err != nil {
		t.Fatalf("PackDir: %v", err)
	}
	if res.Hash != pin {
		t.Errorf("PackDir hash = %s, want the pinned %s", res.Hash, pin)
	}
	opened, cleanup, err := Open(out, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()
	if opened.Hash != pin {
		t.Errorf("the archive reads back at %s, want the pinned %s: the two content walkers disagree", opened.Hash, pin)
	}
	body, err := os.ReadFile(filepath.Join(opened.Dir, filepath.FromSlash(catalogRelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(producer) {
		t.Errorf("the archived catalog = %q, want the producer's %q: a redistributed bundle would carry one workspace's routing table", body, producer)
	}
	if _, err := os.Stat(filepath.Join(opened.Dir, LocalStateFile)); !os.IsNotExist(err) {
		t.Errorf("the archive carries the local-state record (stat err = %v)", err)
	}
}
