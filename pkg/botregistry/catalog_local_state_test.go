package botregistry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// catalogLocalStateRel is the one bundle path a regeneration replaces.
var catalogLocalStateRel = bundle.DirSkills + "/" + catalogGeneratedName

// pinnedOwnerWorkspace vendors a router bundle the way `bots sync`
// materializes one — under <ws>/.botz/<name>, carrying the template AND the
// generated catalogue its producer shipped (catalogSentinel) — plus one
// workspace bot whose row is what makes a live table differ from the frozen
// one. It returns the workspace root and the materialized bundle root.
func pinnedOwnerWorkspace(t *testing.T) (ws, owner string) {
	t.Helper()
	ws = t.TempDir()
	stub := "agent x:\n  model: \"test\"\n"
	owner = filepath.Join(ws, bundle.DirDeps, "routerbot")
	writeFile(t, filepath.Join(owner, bundle.ManifestFile), "name: routerbot\ndisplay_name: Router\ndescription: Routes work.\n")
	writeFile(t, filepath.Join(owner, bundle.MainBotFile), stub)
	writeFile(t, filepath.Join(owner, catalogStaticName), catalogTemplateBody)
	writeFile(t, filepath.Join(owner, bundle.DirSkills, catalogGeneratedName), catalogSentinel)
	addWorkspaceBot(t, ws, "localbot")
	ClearSchemaCache()
	return ws, owner
}

// addWorkspaceBot vendors one more bot under <ws>/bots, which changes the
// routing table every owner renders.
func addWorkspaceBot(t *testing.T, ws, name string) {
	t.Helper()
	dir := filepath.Join(ws, "bots", name)
	writeFile(t, filepath.Join(dir, bundle.ManifestFile), "name: "+name+"\ndisplay_name: P-"+name+"\ndescription: Does work.\n")
	writeFile(t, filepath.Join(dir, bundle.MainBotFile), "agent x:\n  model: \"test\"\n")
	ClearSchemaCache()
}

func hashOrFatal(t *testing.T, dir string) string {
	t.Helper()
	hash, err := bundle.ContentHashDir(dir)
	if err != nil {
		t.Fatalf("ContentHashDir %s: %v", dir, err)
	}
	return hash
}

func readOrFatal(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// TestRegenerate_APinnedOwnerKeepsWhatItShipsAndRecordsTheWrite is the
// writer half of ARB-109's answer: a router bundle materialized under .botz
// is a catalogue owner like any other, so the regeneration gives it the
// CONSUMER workspace's routing table — and bots.lock keeps its pin, because
// the bytes its producer shipped are kept aside and recorded as the local
// state they replaced.
func TestRegenerate_APinnedOwnerKeepsWhatItShipsAndRecordsTheWrite(t *testing.T) {
	ws, owner := pinnedOwnerWorkspace(t)
	pin := hashOrFatal(t, owner)
	generated := filepath.Join(owner, bundle.DirSkills, catalogGeneratedName)

	if _, err := RegenerateWhatsNextCatalog(context.Background(), ws); err != nil {
		t.Fatalf("RegenerateWhatsNextCatalog: %v", err)
	}

	live := readOrFatal(t, generated)
	if live == catalogSentinel {
		t.Fatalf("test premise: the regeneration never reached the materialized owner")
	}
	if !strings.Contains(live, "localbot") {
		t.Errorf("the materialized router's catalog does not carry the workspace's bot:\n%s", live)
	}

	entries, err := bundle.ReadLocalStateOfKind(owner, bundle.LocalStateCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != catalogLocalStateRel {
		t.Fatalf("local-state record = %+v, want the one regenerated catalog line", entries)
	}
	if entries[0].SHA256 != bundle.SHA256Bytes([]byte(live)) {
		t.Errorf("recorded digest = %s, want the digest of the bytes on disk", entries[0].SHA256)
	}
	if entries[0].KeptSHA256 != bundle.SHA256Bytes([]byte(catalogSentinel)) {
		t.Errorf("recorded kept digest = %s, want the digest of the catalog the producer shipped", entries[0].KeptSHA256)
	}
	if kept := readOrFatal(t, filepath.Join(owner, filepath.FromSlash(bundle.KeptPathFor(catalogLocalStateRel)))); kept != catalogSentinel {
		t.Errorf("kept copy = %q, want the bytes the producer shipped", kept)
	}
	if got := hashOrFatal(t, owner); got != pin {
		t.Errorf("ContentHashDir = %s after the refresh, want the pinned %s: every pin check reads this install as drifted", got, pin)
	}
}

// TestRegenerate_TheSecondPassKeepsTheProducersBytesNotItsOwn: the file a
// second regeneration replaces is the FIRST one's output, so taking the live
// bytes every time would keep a derived copy as if its producer had shipped
// it — and the pin would then verify against bytes no producer published.
// The record is what answers "whose bytes are on disk".
func TestRegenerate_TheSecondPassKeepsTheProducersBytesNotItsOwn(t *testing.T) {
	ws, owner := pinnedOwnerWorkspace(t)
	pin := hashOrFatal(t, owner)
	if _, err := RegenerateWhatsNextCatalog(context.Background(), ws); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	first := readOrFatal(t, filepath.Join(owner, bundle.DirSkills, catalogGeneratedName))

	// A new bot in the workspace: the table the second pass renders differs
	// from the one the first pass wrote.
	addWorkspaceBot(t, ws, "secondbot")
	if _, err := RegenerateWhatsNextCatalog(context.Background(), ws); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	second := readOrFatal(t, filepath.Join(owner, bundle.DirSkills, catalogGeneratedName))
	if second == first {
		t.Fatalf("test premise: the second refresh rendered the same table")
	}

	entries, err := bundle.ReadLocalStateOfKind(owner, bundle.LocalStateCatalog)
	if err != nil || len(entries) != 1 {
		t.Fatalf("local-state record = %+v, err=%v", entries, err)
	}
	if entries[0].KeptSHA256 != bundle.SHA256Bytes([]byte(catalogSentinel)) {
		t.Errorf("kept digest after two refreshes = %s, want the producer's: the second pass kept its own output", entries[0].KeptSHA256)
	}
	if kept := readOrFatal(t, filepath.Join(owner, filepath.FromSlash(bundle.KeptPathFor(catalogLocalStateRel)))); kept != catalogSentinel {
		t.Errorf("kept copy after two refreshes = %q, want the bytes the producer shipped", kept)
	}
	if got := hashOrFatal(t, owner); got != pin {
		t.Errorf("ContentHashDir after two refreshes = %s, want the pinned %s", got, pin)
	}
}

// TestRegenerate_ASourceTreeOwnerShipsItsGeneratedCatalog: an owner in its
// own checkout is the PRODUCER of that file — `task gen` keeps it true and
// the packer must ship it. Recording it as local state there would hide a
// committed artefact from the hash and from every archive.
func TestRegenerate_ASourceTreeOwnerShipsItsGeneratedCatalog(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	owner := filepath.Join(ws, "bots", "whats-next")
	before := hashOrFatal(t, owner)

	if _, err := RegenerateWhatsNextCatalog(context.Background(), ws); err != nil {
		t.Fatalf("RegenerateWhatsNextCatalog: %v", err)
	}

	if _, err := os.Stat(filepath.Join(owner, bundle.LocalStateFile)); !os.IsNotExist(err) {
		t.Errorf("a source-tree owner carries a local-state record (stat err = %v): its generated catalog is content it ships", err)
	}
	if got := hashOrFatal(t, owner); got == before {
		t.Errorf("ContentHashDir = %s, unchanged: the regeneration either did not write, or its write was hidden from the bundle's own content", got)
	}
}

// TestRegenerate_AnOwnerOutsideThePinnedRootLosesItsCatalogRecord: a bundle
// an operator moved out of .botz has no pin left, so a record that hides its
// generated catalogue from the hash would describe a bundle nobody pins —
// and would survive every later refresh. The refresh that finds it removes
// the line it owns.
func TestRegenerate_AnOwnerOutsideThePinnedRootLosesItsCatalogRecord(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	owner := filepath.Join(ws, "bots", "whats-next")
	stale := bundle.SHA256Bytes([]byte(catalogSentinel)) + " " + catalogLocalStateRel + " " + bundle.SHA256Bytes([]byte("whatever the producer shipped\n")) + "\n"
	writeFile(t, filepath.Join(owner, bundle.LocalStateFile), stale)

	if _, err := RegenerateWhatsNextCatalog(context.Background(), ws); err != nil {
		t.Fatalf("RegenerateWhatsNextCatalog: %v", err)
	}

	if _, err := os.Stat(filepath.Join(owner, bundle.LocalStateFile)); !os.IsNotExist(err) {
		t.Errorf("the stale catalog record survived the refresh (stat err = %v)", err)
	}
}

// TestRegenerate_APinnedOwnerThatShipsNoGeneratedCatalog: a producer may
// leave its generated catalogue out of what it publishes (it is derived, and
// a repository can ignore it). The regeneration then ADDS a path no producer
// shipped — the fragment case — so the record keeps nothing and the file is
// left out of the hash entirely.
func TestRegenerate_APinnedOwnerThatShipsNoGeneratedCatalog(t *testing.T) {
	ws, owner := pinnedOwnerWorkspace(t)
	generated := filepath.Join(owner, bundle.DirSkills, catalogGeneratedName)
	if err := os.Remove(generated); err != nil {
		t.Fatal(err)
	}
	pin := hashOrFatal(t, owner)

	if _, err := RegenerateWhatsNextCatalog(context.Background(), ws); err != nil {
		t.Fatalf("RegenerateWhatsNextCatalog: %v", err)
	}

	if live := readOrFatal(t, generated); !strings.Contains(live, "localbot") {
		t.Errorf("the regeneration wrote no live table:\n%s", live)
	}
	entries, err := bundle.ReadLocalStateOfKind(owner, bundle.LocalStateCatalog)
	if err != nil || len(entries) != 1 {
		t.Fatalf("local-state record = %+v, err=%v; want the one added catalog line", entries, err)
	}
	if entries[0].KeptSHA256 != "" {
		t.Errorf("kept digest = %s, want none: there were no producer bytes to keep", entries[0].KeptSHA256)
	}
	if _, err := os.Stat(filepath.Join(owner, filepath.FromSlash(bundle.KeptPathFor(catalogLocalStateRel)))); !os.IsNotExist(err) {
		t.Errorf("a kept copy was written for a file the producer never shipped (stat err = %v)", err)
	}
	if got := hashOrFatal(t, owner); got != pin {
		t.Errorf("ContentHashDir = %s, want the pinned %s: an added path must be left out of the hash", got, pin)
	}
}
