package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The consumer bundle as the CLOUD editor holds it: a files map that
// declares the shared fragment and does not carry it, whose workflow USES a
// declaration the fragment provides (`output: shared_state`). Without the
// fragment this bundle draws E046 for the import AND the compile errors for
// the node that references what the fragment declares — the pair #2865
// measured — so a witness here cannot pass by tolerating the import alone.
func fragmentConsumerFiles(extraDeps ...string) map[string]string {
	// The cloud push holds a bundle that uses `import` to the engine floor
	// its manifest must declare: the witness is about fragments, so the
	// floor is stated and the guard it belongs to is left alone.
	manifest := fragmentConsumerManifest + strings.Join(extraDeps, "") +
		"requires:\n  iterion: \">= " + parser.ImportSince + "\"\n"
	return map[string]string{
		botsource.MainBotFile: fragmentConsumerMain,
		bundle.ManifestFile:   manifest,
	}
}

// writeFragmentProducer lays out the producer bundle alone, as the baked
// catalog tier serves it.
func writeFragmentProducer(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, "producer", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// putBundle is the whole-bundle write gate — the push that refused a bundle
// declaring a shared fragment with `E046 … no such fragment`, C001 and C008.
func putFragmentBundle(t *testing.T, s *Server, id auth.Identity, slug string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(botSourcePutReq{Files: files})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/api/teams/t1/bot-sources/"+slug, bytes.NewReader(body)).
		WithContext(auth.WithIdentity(context.Background(), id))
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	return w
}

// parseCallAs is parseCall with an identity on the request: the authoring
// plane's resolution is team-scoped, so who asks decides whether a tier is
// read at all.
func parseCallAs(t *testing.T, s *Server, id auth.Identity, body map[string]any) (*httptest.ResponseRecorder, parseResponse) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/parse", bytes.NewReader(raw)).
		WithContext(auth.WithIdentity(context.Background(), id))
	w := httptest.NewRecorder()
	s.handleParse(w, r)
	var out parseResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode parse: %v\n%s", err, w.Body.String())
		}
	}
	return w, out
}

// TestTheCloudWriteGateCompilesABundleThatDeclaresItsFragment is the
// ticket's wall, from the author's side: a bundle that DECLARES a shared
// fragment instead of shipping it is launchable (327c064d6 resolves it in
// the freeze) and was refused by the authoring surface — so the state the
// launch fix helps could never be reached through the editor. The gate now
// resolves what the launch resolves, through the same producer chokepoint.
func TestTheCloudWriteGateCompilesABundleThatDeclaresItsFragment(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifest,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}

	rec := putFragmentBundle(t, s, editor, "consumer", fragmentConsumerFiles())
	if rec.Code != http.StatusOK {
		t.Fatalf("a bundle declaring a shared fragment its producer exports: %d %s", rec.Code, rec.Body.String())
	}
	// The bundle is stored as the AUTHOR wrote it: the resolved fragment is
	// read for the compile and never persisted, or the bundle would ship the
	// very lib/<name>.bot the launch refuses as a shadow.
	stored, err := s.botSources.GetBySlug(context.Background(), "t1", "consumer")
	if err != nil {
		t.Fatalf("read back the stored bundle: %v", err)
	}
	if _, ok := stored.Files["lib/forge-auth.bot"]; ok {
		t.Fatalf("the write gate persisted the resolved fragment: %v", sortedFileKeys(stored.Files))
	}
}

// TestTheCloudWriteGateStillRefusesAProducerNoTierServes: the naming half
// #2865 landed is not deleted by the resolution, it is re-pointed. A
// producer nowhere is a declaration error, permanent, and the refusal names
// the dependency, the producer and the TIERS searched — never `iterion bots
// sync`, which is no way forward for a server with no working tree
// (exportedFragmentForSnapshot's own reason, on the launch plane).
func TestTheCloudWriteGateStillRefusesAProducerNoTierServes(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	s.cfg.Bots.Paths = []string{filepath.Join(t.TempDir(), "bots")}

	rec := putFragmentBundle(t, s, editor, "consumer", fragmentConsumerFiles())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a bundle whose producer is nowhere: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"dependencies.fragments", "forge-auth", "producer", "no tier of this server", "publish"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the refusal does not name %q (%d):\n%s", want, rec.Code, body)
		}
	}
	if strings.Contains(body, "iterion bots sync") {
		t.Fatalf("the refusal sends a cloud author to a working tree the server has not got:\n%s", body)
	}
}

// TestTheCloudWriteGateRefusesADependencyThatShadowsAShippedFragment:
// ADR-145 clause 2's no-shadow verdict is the launch's, and the authoring
// plane holds it over the bundle it is about to resolve — or the editor
// blesses a bundle no launch takes. The bundle here declares two fragments:
// one is missing (what triggers the resolution) and one it SHIPS with bytes
// its own record does not describe.
func TestTheCloudWriteGateRefusesADependencyThatShadowsAShippedFragment(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifest,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}

	files := fragmentConsumerFiles("    - name: app-policy\n      from: producer\n")
	files["lib/app-policy.bot"] = "schema policy_state:\n  ok: bool\n"

	rec := putFragmentBundle(t, s, editor, "consumer", files)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a dependency shadowing a fragment the bundle ships: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "would shadow") {
		t.Fatalf("shadow refusal expected:\n%s", rec.Body.String())
	}
}

// TestNoTierIsReadUntilACompileMissesADeclaredFragment is the cost
// argument, witnessed rather than asserted: the producer tier is POISONED
// (a bundle that exports no fragment at all), so any tier read at all turns
// into a refusal naming it. A bundle pushed from a synced working tree —
// the fragment materialized, the bundle's own record describing it, which
// botsource.ReadBundleDir carries into the cloud — has no declared miss, so
// it must pass untouched. Strip the materialized file and the same bundle
// DOES read the tier, which is how we know the poison bites.
func TestNoTierIsReadUntilACompileMissesADeclaredFragment(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile: "name: producer\nversion: 1.0.0\n",
		"main.bot":          testBotMain,
	})
	s.cfg.Bots.Paths = []string{root}

	synced := fragmentConsumerFiles()
	synced["lib/forge-auth.bot"] = sharedFragmentBody
	// The record's own line shape: "<sha256> <path>" (pkg/bundle).
	synced[bundle.LocalStateFile] = bundle.SHA256Bytes([]byte(sharedFragmentBody)) + " lib/forge-auth.bot\n"
	if rec := putFragmentBundle(t, s, editor, "synced", synced); rec.Code != http.StatusOK {
		t.Fatalf("a bundle whose declared fragment is already materialized read a tier it had no business reading: %d %s", rec.Code, rec.Body.String())
	}

	// The control: the same bundle short of the materialized file reaches
	// the tier, and the poison answers.
	short := fragmentConsumerFiles()
	rec := putFragmentBundle(t, s, editor, "short", short)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the poisoned producer was not consulted for a declared miss: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exports no fragment") {
		t.Fatalf("the refusal is not the poisoned tier's:\n%s", rec.Body.String())
	}
}

// TestAResolutionNeedsTheCallersTeamRight: /api/parse is authenticated but
// takes a caller-supplied files map, so resolving a producer the map NAMES
// would turn the route into a cross-team read primitive — the producer's
// fragment text lands inside the merged document the response echoes. The
// team is the editor path's and the caller must hold it; without the right,
// no tier is read and the naming refusal stands.
func TestAResolutionNeedsTheCallersTeamRight(t *testing.T) {
	s, editor, viewer := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifest,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}
	body := map[string]any{
		"files": fragmentConsumerFiles(),
		"main":  botsource.MainBotFile,
		"path":  botSourceScheme + "t1/consumer/" + botsource.MainBotFile,
	}

	// The caller who holds the team gets the dependency's declarations.
	rec, out := parseCallAs(t, s, editor, body)
	if rec.Code != http.StatusOK || out.Unit == nil {
		t.Fatalf("the team's own editor: %d %s", rec.Code, rec.Body.String())
	}
	if out.Unit.LoadError != "" {
		t.Fatalf("the resolved unit still does not load: %q", out.Unit.LoadError)
	}
	if !documentDeclares(t, out.Document, sharedFragmentDeclaration) {
		t.Fatalf("the resolved document lacks the fragment's declaration %q", sharedFragmentDeclaration)
	}

	// A viewer of the same team holds no right on its bots (canEditBots):
	// no tier is read, and no byte of the producer reaches the answer.
	rec, out = parseCallAs(t, s, viewer, body)
	if rec.Code != http.StatusOK || out.Unit == nil {
		t.Fatalf("a viewer: %d %s", rec.Code, rec.Body.String())
	}
	if out.Unit.LoadError == "" {
		t.Fatal("a caller with no right on the team got the dependency resolved")
	}
	if documentDeclares(t, out.Document, sharedFragmentDeclaration) {
		t.Fatalf("a caller with no right on the team got the producer's fragment text")
	}

	// And an identity whose own team is NOT the path's: the rule
	// s.mergeBotSourcePrompts holds a stored-bundle read to, on this plane.
	other := editor
	other.TeamID = "t2"
	rec, out = parseCallAs(t, s, other, body)
	if rec.Code != http.StatusOK || out.Unit == nil {
		t.Fatalf("another team's caller: %d %s", rec.Code, rec.Body.String())
	}
	if documentDeclares(t, out.Document, sharedFragmentDeclaration) {
		t.Fatalf("another team's caller got this team's producer text")
	}
}

// TestNoSaveWritesTheResolvedFragmentBack: a resolved fragment is a file of
// the unit, so the picker lists it and the save would write every changed
// file back into the bundle — which would make the bundle SHIP
// lib/<name>.bot, the shadow the launch refuses. The assertion is on the
// files the save hands back, never on the status code: those are what the
// studio patches into the versioned PUT.
func TestNoSaveWritesTheResolvedFragmentBack(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifest,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}
	files := fragmentConsumerFiles()
	editorPath := botSourceScheme + "t1/consumer/" + botsource.MainBotFile

	rec, opened := parseCallAs(t, s, editor, map[string]any{"files": files, "main": botsource.MainBotFile, "path": editorPath})
	if rec.Code != http.StatusOK || opened.Unit == nil {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := unitFileNamed(opened.Unit, "lib/forge-auth.bot"); !ok {
		t.Fatalf("the resolved fragment is no file of the opened unit: %+v", opened.Unit.Files)
	}
	if fi, _ := unitFileNamed(opened.Unit, "lib/forge-auth.bot"); fi.Dependency != "producer" {
		t.Fatalf("the resolved file is not marked as the producer's: %+v", fi)
	}

	// The save of the document as opened: nothing of the bundle changed, and
	// above all the resolved fragment is not among the files handed back.
	rec, saved := unparseCallAs(t, s, editor, map[string]any{
		"document": opened.Document,
		"files":    files,
		"main":     botsource.MainBotFile,
		"revision": opened.Unit.Revision,
		"path":     editorPath,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("the save of an untouched resolved unit: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := saved.Files["lib/forge-auth.bot"]; ok {
		t.Fatalf("the save hands back the resolved fragment, which the studio would patch into the bundle: %v", sortedFileKeys(saved.Files))
	}

	// And an edit that WOULD rewrite it is refused by name — the author's
	// own words, not a silent 200 that keeps none of the change.
	stripped := documentWithout(t, opened.Document, "lib/forge-auth.bot")
	rec, rewritten := unparseCallAs(t, s, editor, map[string]any{
		"document": stripped,
		"files":    files,
		"main":     botsource.MainBotFile,
		"revision": opened.Unit.Revision,
		"path":     editorPath,
	})
	// What the save HANDS BACK is asserted before its status: those bytes
	// are what the studio patches into its versioned PUT, so a 200 carrying
	// the fragment is the bundle shipping the shadow, whatever the code says.
	if _, ok := rewritten.Files["lib/forge-auth.bot"]; ok {
		t.Fatalf("a save rewrote the resolved fragment, which the studio would patch into the bundle: %v", sortedFileKeys(rewritten.Files))
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a save that rewrites the resolved fragment: %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"lib/forge-auth.bot", "producer", "dependencies.fragments"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, rec.Body.String())
		}
	}
}

// unparseCallAs is unparseCall with an identity on the request.
func unparseCallAs(t *testing.T, s *Server, id auth.Identity, body map[string]any) (*httptest.ResponseRecorder, unparseResponse) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/unparse", bytes.NewReader(raw)).
		WithContext(auth.WithIdentity(context.Background(), id))
	w := httptest.NewRecorder()
	s.handleUnparse(w, r)
	var out unparseResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode unparse: %v\n%s", err, w.Body.String())
		}
	}
	return w, out
}

// documentDeclares reports whether the answered document carries a schema of
// that name: what proves the fragment reached the MERGED program, not merely
// that a file was injected somewhere.
func documentDeclares(t *testing.T, raw json.RawMessage, name string) bool {
	t.Helper()
	if len(raw) == 0 {
		return false
	}
	f, err := ast.UnmarshalFile(raw)
	if err != nil {
		return false
	}
	for _, schema := range f.Schemas {
		if schema.Name == name {
			return true
		}
	}
	return false
}

// documentWithout is the opened document with every declaration whose
// provenance names rel removed: the shape a save that rewrites that file
// takes, since the split by provenance then finds nothing of it.
func documentWithout(t *testing.T, raw json.RawMessage, rel string) json.RawMessage {
	t.Helper()
	f, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatalf("decode the opened document: %v", err)
	}
	kept := make([]*ast.SchemaDecl, 0, len(f.Schemas))
	for _, schema := range f.Schemas {
		if schema.Span.Start.File == rel {
			continue
		}
		kept = append(kept, schema)
	}
	if len(kept) == len(f.Schemas) {
		t.Fatalf("the opened document declares nothing from %s, so this is no witness", rel)
	}
	f.Schemas = kept
	out, err := ast.MarshalFileWithProvenance(f, "")
	if err != nil {
		t.Fatalf("re-encode the document: %v", err)
	}
	return out
}

// unitFileNamed is the answered unit's entry for rel.
func unitFileNamed(info *unitInfo, rel string) (unitFileInfo, bool) {
	for _, fi := range info.Files {
		if fi.Rel == rel {
			return fi, true
		}
	}
	return unitFileInfo{}, false
}

// sortedFileKeys names a files map in a stable order, for a failure message
// a reader can compare between runs.
func sortedFileKeys(files map[string]string) []string {
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	return rels
}

// sharedBaseFragmentBody is a SECOND shared fragment, which the first one
// imports: the shape a fragment library actually takes once it is shared.
const sharedBaseFragmentBody = "schema shared_base:\n  trace: string\n"

// fragmentProducerManifestTwo exports both, and forgeAuthImportingBase is
// the first one importing the second — `import "shared-base.bot"` resolves
// beside the importing fragment, which is the bundle's own lib/.
const fragmentProducerManifestTwo = "name: producer\nversion: 1.0.0\nexports:\n  fragments:\n    - id: forge-auth\n      path: lib/forge-auth.bot\n    - id: shared-base\n      path: lib/shared-base.bot\n"

const forgeAuthImportingBase = "import \"shared-base.bot\"\n" + sharedFragmentBody

// TestTheCloudGateResolvesAFragmentThatImportsASecondDeclaredFragment: the
// resolution's shape is the LAUNCH's — resolveSnapshotFragments loops the
// manifest's entries, all of them — and not "whatever the first reading
// missed". A shared fragment that itself imports a second shared fragment is
// the realistic library, it is launchable, and a gate resolving only the
// first reading's misses refuses it one level deeper: the very wall #2865 is
// about. The trigger stays lazy; what changes is the SCOPE once it fires.
func TestTheCloudGateResolvesAFragmentThatImportsASecondDeclaredFragment(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:   fragmentProducerManifestTwo,
		"main.bot":            testBotMain,
		"lib/forge-auth.bot":  forgeAuthImportingBase,
		"lib/shared-base.bot": sharedBaseFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}
	files := fragmentConsumerFiles("    - name: shared-base\n      from: producer\n")

	if rec := putFragmentBundle(t, s, editor, "consumer", files); rec.Code != http.StatusOK {
		t.Fatalf("the gate refuses a declared fragment that imports a second declared fragment: %d %s", rec.Code, rec.Body.String())
	}
	// Both declarations reached the MERGED program, which is what proves the
	// second entry was resolved rather than merely tolerated.
	rec, opened := parseCallAs(t, s, editor, map[string]any{
		"files": files, "main": botsource.MainBotFile,
		"path": botSourceScheme + "t1/consumer/" + botsource.MainBotFile,
	})
	if rec.Code != http.StatusOK || opened.Unit == nil {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	if opened.Unit.LoadError != "" {
		t.Fatalf("the transitively resolved unit does not load: %q", opened.Unit.LoadError)
	}
	for _, want := range []string{sharedFragmentDeclaration, "shared_base"} {
		if !documentDeclares(t, opened.Document, want) {
			t.Fatalf("the resolved document lacks %q", want)
		}
	}
}

// TestTheCloudWriteGateRefusesASingleDeclaredShadow: the no-shadow verdict's
// trigger must be the DECLARATION, not "something else of this bundle is
// missing". A bundle whose only declared entry is also the file it ships
// misses nothing, so a guard behind the lazy trigger never runs on the one
// bundle it is about — and the gate stores what the launch refuses. The
// oracle is the launch's own verdict function, so the two planes cannot be
// read as agreeing when they do not.
func TestTheCloudWriteGateRefusesASingleDeclaredShadow(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifest,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}

	files := fragmentConsumerFiles()
	// Shipped, with no record of a materialization: the bundle's own file.
	files["lib/forge-auth.bot"] = sharedFragmentBody
	launch := botinstall.RefuseFragmentShadow("lib/forge-auth.bot", bundle.SHA256Bytes([]byte(sharedFragmentBody)), true,
		bundle.FragmentDependency{Name: "forge-auth", From: "producer"}, nil)
	if launch == nil {
		t.Fatal("the launch takes this bundle, so this is no witness")
	}
	rec := putFragmentBundle(t, s, editor, "consumer", files)
	if rec.Code == http.StatusOK {
		t.Fatalf("the gate stored a bundle the launch refuses (%v): %s", launch, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "would shadow") {
		t.Fatalf("the refusal is not the launch's no-shadow verdict:\n%s", rec.Body.String())
	}
	// And an edit of a MATERIALIZED dependency — the file a synced tree
	// carries with its record — is the same shadow once the bytes no longer
	// match the record, which is the journey a cloud save takes.
	edited := fragmentConsumerFiles()
	edited["lib/forge-auth.bot"] = sharedFragmentBody + "\nschema drifted:\n  v: string\n"
	edited[bundle.LocalStateFile] = bundle.SHA256Bytes([]byte(sharedFragmentBody)) + " lib/forge-auth.bot\n"
	if rec := putFragmentBundle(t, s, editor, "edited", edited); rec.Code == http.StatusOK {
		t.Fatalf("the gate stored an edited dependency file whose record no longer describes it: %s", rec.Body.String())
	}
}

// TestAProducerThatCannotServeTheFragmentStillOpens: a typo in a
// `dependencies.fragments` id, a producer that renamed its export, a
// producer that exports nothing — each is a permanent DECLARATION error, and
// each must leave the consumer OPENABLE. A resolver refusal on the read path
// makes every consumer's main.bot impossible to open in the studio and an
// in-flight edit unsavable, so the author cannot reach the screen where the
// manifest is fixed: #2865's first half asks the opposite, and so does
// philosophy clause 1 ("warn rather than reject"). The write gate still
// refuses — the import the declaration exists for does not resolve.
func TestAProducerThatCannotServeTheFragmentStillOpens(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		// Resolves as a bundle, exports nothing.
		bundle.ManifestFile: "name: producer\nversion: 1.0.0\n",
		"main.bot":          testBotMain,
	})
	s.cfg.Bots.Paths = []string{root}
	files := fragmentConsumerFiles()

	rec, opened := parseCallAs(t, s, editor, map[string]any{
		"files": files, "main": botsource.MainBotFile,
		"path": botSourceScheme + "t1/consumer/" + botsource.MainBotFile,
	})
	if rec.Code != http.StatusOK || opened.Unit == nil {
		t.Fatalf("the open of a consumer whose producer cannot serve the fragment: %d %s", rec.Code, rec.Body.String())
	}
	// The miss is READABLE where the author works: the unit's own verdict,
	// naming the manifest entry, the producer and what the resolution found.
	for _, want := range []string{"dependencies.fragments", "forge-auth", "producer", "exports no fragment"} {
		if !strings.Contains(opened.Unit.LoadError, want) {
			t.Fatalf("the opened unit's verdict does not name %q:\n%s", want, opened.Unit.LoadError)
		}
	}
	if rec := putFragmentBundle(t, s, editor, "consumer", files); rec.Code != http.StatusBadRequest {
		t.Fatalf("the write gate took a bundle whose declared fragment resolves nowhere: %d %s", rec.Code, rec.Body.String())
	}
}

// TestARefusalNamesWhatTHISEntrysResolutionFound: a remedy stamped over the
// whole declaration set lies about every entry it is not about. A producer
// that RESOLVED and cannot serve the id is not "a producer that resolves in
// no tier", and telling the author to publish it is a dead end; nor is
// `iterion bots sync` a way forward for a server with no working tree.
func TestARefusalNamesWhatTHISEntrysResolutionFound(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile: "name: producer\nversion: 1.0.0\n",
		"main.bot":          testBotMain,
	})
	s.cfg.Bots.Paths = []string{root}

	rec := putFragmentBundle(t, s, editor, "consumer", fragmentConsumerFiles())
	body := rec.Body.String()
	if !strings.Contains(body, "exports no fragment") {
		t.Fatalf("the refusal does not say what the resolution found:\n%s", body)
	}
	for _, forbidden := range []string{"resolves in no tier of this server", "iterion bots sync"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the refusal claims %q about a producer that RESOLVED:\n%s", forbidden, body)
		}
	}
	// And the server's own filesystem path never leaves the server — the
	// disclosure #1934 cut out of every unit diagnostic. In production that
	// path is the baked-catalog mount or a tier materialization's temp dir.
	if strings.Contains(body, root) {
		t.Fatalf("the refusal discloses the server path %q:\n%s", root, body)
	}
}

// TestACloudReadWithNoRightSaysWhatThisRequestKnows: the plane a refusal
// belongs to is the PATH's, not the caller's right — `iterion bots sync` is
// no way forward for a server with no working tree. But the plane's OTHER
// sentence is not free either: a request the resolution refused read no tier,
// so "the producer resolves in no tier of this server: publish it or drop the
// dependency" is a verdict it never earned, and the first half of this witness
// proves it false on the same server, producer and bundle. A sentence that
// told the author "the producer is nowhere" about a producer that answers is
// worse than no sentence, and this one also advises a destructive edit on a
// correct manifest.
func TestACloudReadWithNoRightSaysWhatThisRequestKnows(t *testing.T) {
	s, editor, viewer := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifest,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}

	body := map[string]any{
		"files": fragmentConsumerFiles(), "main": botsource.MainBotFile,
		"path": botSourceScheme + "t1/consumer/" + botsource.MainBotFile,
	}
	// The producer DOES resolve and serve, for the team's own editor, on this
	// very server: without that the sentence under test would be true and this
	// would be no witness.
	if rec, served := parseCallAs(t, s, editor, body); rec.Code != http.StatusOK || served.Unit == nil || served.Unit.LoadError != "" {
		t.Fatalf("the producer does not serve the team's editor, so this is no witness: %d %q", rec.Code, rec.Body.String())
	}
	rec, opened := parseCallAs(t, s, viewer, body)
	if rec.Code != http.StatusOK || opened.Unit == nil {
		t.Fatalf("a viewer's open: %d %s", rec.Code, rec.Body.String())
	}
	if opened.Unit.LoadError == "" {
		t.Fatal("a caller with no right on the team got the dependency resolved")
	}
	for _, forbidden := range []string{
		// The working tree the server has not got.
		"iterion bots sync",
		// A verdict about a producer this request never looked for, and
		// the edit it advises would delete a correct declaration.
		"resolves in no tier of this server",
		"drop the dependency (ADR-145)",
	} {
		if strings.Contains(opened.Unit.LoadError, forbidden) {
			t.Fatalf("the refusal claims %q about a producer this server resolves and serves:\n%s", forbidden, opened.Unit.LoadError)
		}
	}
	// And it still names the declaration behind the miss, with what IS true.
	for _, want := range []string{"dependencies.fragments", "forge-auth", "did not resolve it on this request"} {
		if !strings.Contains(opened.Unit.LoadError, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, opened.Unit.LoadError)
		}
	}
}

// TestTheAssistantGateResolvesWhatTheRouteResolves: the assistant's
// authoring plane is a write gate like the bot-source routes, and a class
// fixed at one of two gates is not fixed. Without the resolution the
// assistant can change NO file of any bot whose bundle declares a shared
// fragment — every preview refused with the E046 the declaration exists for
// — while a human PUT of the same bytes is accepted and the launch runs it.
func TestTheAssistantGateResolvesWhatTheRouteResolves(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifest,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}
	files := fragmentConsumerFiles()

	// The route takes these bytes, so a refusal below is a plane
	// disagreement and not a bad bundle.
	if rec := putFragmentBundle(t, s, editor, "consumer", files); rec.Code != http.StatusOK {
		t.Fatalf("the route refuses the bundle, so this is no witness: %d %s", rec.Code, rec.Body.String())
	}
	target := &authoringTarget{s: s, files: files, teamID: "t1", slug: "consumer"}
	if err := target.validateChangedBots(context.Background(), []authoringPreviewFile{{
		Scope: bundle.AuthoringScopeBundle, Path: botsource.MainBotFile, After: fragmentConsumerMain,
	}}); err != nil {
		t.Fatalf("the assistant refuses what the route accepts and the launch runs: %v", err)
	}
	// The no-shadow verdict holds on this gate too, or the assistant is the
	// way round it.
	shadowing := fragmentConsumerFiles()
	shadowing["lib/forge-auth.bot"] = sharedFragmentBody
	shadow := &authoringTarget{s: s, files: shadowing, teamID: "t1", slug: "consumer"}
	if err := shadow.validateChangedBots(context.Background(), []authoringPreviewFile{{
		Scope: bundle.AuthoringScopeBundle, Path: botsource.MainBotFile, After: fragmentConsumerMain,
	}}); err == nil || !strings.Contains(err.Error(), "would shadow") {
		t.Fatalf("the assistant gate does not hold the no-shadow verdict: %v", err)
	}
}

// TestATierOutageIsTransientForTheWriteGate: a tier that did not answer is
// not "the bundle does not compile". Flattened into the diagnostics it reads
// as a permanent refusal, and one bad Mongo minute retires a bundle that
// resolves — the façade pkg/server/bot_resolver.go refuses elsewhere for the
// same reason. The read plane already answers 503 for this class
// (unitRequestError); the write plane must not disagree.
func TestATierOutageIsTransientForTheWriteGate(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	s.cfg.Bots.Paths = []string{filepath.Join(t.TempDir(), "bots")}
	s.botSources = fragmentProducerOutageStore{Store: s.botSources}

	rec := putFragmentBundle(t, s, editor, "consumer", fragmentConsumerFiles())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a store outage while resolving the producer: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "does not compile") {
		t.Fatalf("a tier outage is answered as a compile failure:\n%s", rec.Body.String())
	}
}

// TestADeleteCanUndoAShadowAndCannotCreateOne: the delete route's scope rule
// — "a diagnostic the bundle drew WITH the file is not this delete's" —
// covers the resolution's refusals too, and it has to: deleting the shipped
// file is THE way out of a bundle whose declared dependency shadows it, so a
// gate refusing on the BEFORE reading wedges the author inside the state the
// delete repairs. The other direction still holds: a delete that CREATES the
// shadow — dropping the bundle's record of what an install materialized — is
// refused, because the launch refuses what it leaves behind.
func TestADeleteCanUndoAShadowAndCannotCreateOne(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifest,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
	})
	s.cfg.Bots.Paths = []string{root}

	// The shadow state, stored past the gate the way a push from before this
	// rule left it: the store write, not the route.
	shadowing := fragmentConsumerFiles()
	shadowing["lib/forge-auth.bot"] = sharedFragmentBody
	ctx := store.WithTenant(context.Background(), "t1")
	if _, err := s.botSources.Create(ctx, botsource.BotSource{TenantID: "t1", Slug: "shadowed", Files: shadowing}); err != nil {
		t.Fatal(err)
	}
	if rec := deleteFragmentBundleFile(t, s, editor, "shadowed", "lib/forge-auth.bot"); rec.Code != http.StatusOK {
		t.Fatalf("the delete that undoes the shadow is refused: %d %s", rec.Code, rec.Body.String())
	}

	// And the delete that CREATES one: the bundle carries the fragment with
	// the record describing it, which is what makes it an install's output
	// rather than a file of its own.
	synced := fragmentConsumerFiles()
	synced["lib/forge-auth.bot"] = sharedFragmentBody
	synced[bundle.LocalStateFile] = bundle.SHA256Bytes([]byte(sharedFragmentBody)) + " lib/forge-auth.bot\n"
	if rec := putFragmentBundle(t, s, editor, "synced", synced); rec.Code != http.StatusOK {
		t.Fatalf("a synced bundle: %d %s", rec.Code, rec.Body.String())
	}
	rec := deleteFragmentBundleFile(t, s, editor, "synced", bundle.LocalStateFile)
	if rec.Code == http.StatusOK {
		t.Fatalf("the delete of the materialization record left a bundle the launch refuses: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "would shadow") {
		t.Fatalf("the refusal is not the no-shadow verdict:\n%s", rec.Body.String())
	}
}

// deleteFragmentBundleFile is the per-file delete route, as the files drawer
// calls it.
func deleteFragmentBundleFile(t *testing.T, s *Server, id auth.Identity, slug, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/api/teams/t1/bot-sources/"+slug+"/files/"+path, nil).
		WithContext(auth.WithIdentity(context.Background(), id))
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", slug)
	r.SetPathValue("path", path)
	w := httptest.NewRecorder()
	s.handleDeleteBotSourceFile(w, r)
	return w
}

// fragmentProducerManifestBoth exports two fragments, and
// fragmentConsumerMainImportingBoth imports both: the shape that lets a
// witness show one declared entry RESOLVED while the other is left to the
// bundle's own file, which is what tells "the shadow was skipped" from "no
// resolution ran at all".
const fragmentProducerManifestBoth = "name: producer\nversion: 1.0.0\nexports:\n  fragments:\n    - id: forge-auth\n      path: lib/forge-auth.bot\n    - id: app-policy\n      path: lib/app-policy.bot\n"

const fragmentConsumerMainImportingBoth = "import \"lib/forge-auth.bot\"\nimport \"lib/app-policy.bot\"\n\ncompute gate:\n  output: shared_state\n  expr:\n    ok: \"true\"\n    fail_log: \"''\"\n\nworkflow consumer:\n  entry: gate\n"

const producersPolicyBody = "schema producers_policy:\n  ok: bool\n"

const authorsOwnPolicyBody = "schema the_authors_own_policy:\n  mine: bool\n"

// writeFragmentProducerBoth lays out the producer serving both exports.
func writeFragmentProducerBoth(t *testing.T, root string) {
	t.Helper()
	writeFragmentProducer(t, root, map[string]string{
		bundle.ManifestFile:  fragmentProducerManifestBoth,
		"main.bot":           testBotMain,
		"lib/forge-auth.bot": sharedFragmentBody,
		"lib/app-policy.bot": producersPolicyBody,
	})
}

// fragmentConsumerDeclaringBoth declares both entries and imports both. own
// names the declared paths the bundle SHIPS itself, with the bytes it ships
// there — a shadow per ADR-145 clause 2 unless a record accounts for them.
func fragmentConsumerDeclaringBoth(own map[string]string) map[string]string {
	files := fragmentConsumerFiles("    - name: app-policy\n      from: producer\n")
	files[botsource.MainBotFile] = fragmentConsumerMainImportingBoth
	for rel, body := range own {
		files[rel] = body
	}
	return files
}

// TestAnOpenNeverReplacesAFileTheBundleShips: the no-shadow verdict is the
// LAUNCH's, and the read plane holding none of it does not merely miss a
// refusal — it OVERWRITES. A declared entry whose path the bundle ships is
// walked like any other once a resolution fires, so the opened document is
// built on the PRODUCER's bytes instead of the author's file, the unit answers
// that file as the dependency's (the studio renders a dependency read-only),
// and the revision is a digest over bytes the bundle does not contain. The
// state is a supported input by this surface's own design — a bundle already
// shadowing must still OPEN — so it is exactly the one the open must not
// corrupt.
func TestAnOpenNeverReplacesAFileTheBundleShips(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducerBoth(t, root)
	s.cfg.Bots.Paths = []string{root}

	// app-policy is shipped by the bundle with no record: the launch's own
	// verdict refuses it, which is what makes this the state under test.
	// forge-auth is a plain miss, so a resolution DOES fire.
	files := fragmentConsumerDeclaringBoth(map[string]string{"lib/app-policy.bot": authorsOwnPolicyBody})
	launch := botinstall.RefuseFragmentShadow("lib/app-policy.bot", bundle.SHA256Bytes([]byte(authorsOwnPolicyBody)), true,
		bundle.FragmentDependency{Name: "app-policy", From: "producer"}, nil)
	if launch == nil {
		t.Fatal("the launch takes this bundle, so this is no witness")
	}

	rec, opened := parseCallAs(t, s, editor, map[string]any{
		"files": files, "main": botsource.MainBotFile,
		"path": botSourceScheme + "t1/consumer/" + botsource.MainBotFile,
	})
	if rec.Code != http.StatusOK || opened.Unit == nil {
		t.Fatalf("the open of a bundle that shadows one of its declared entries: %d %s", rec.Code, rec.Body.String())
	}
	// A resolution ran — the other entry is placed and marked — so what
	// follows is the shadow being skipped and not a resolution that never
	// fired.
	if fi, ok := unitFileNamed(opened.Unit, "lib/forge-auth.bot"); !ok || fi.Dependency != "producer" {
		t.Fatalf("the declared miss was not resolved, so this witness proves nothing about the shadow: %+v", opened.Unit.Files)
	}
	if !documentDeclares(t, opened.Document, "the_authors_own_policy") {
		t.Fatalf("the opened document does not declare the author's OWN schema: the open built it on another bundle's bytes:\n%s", rec.Body.String())
	}
	if documentDeclares(t, opened.Document, "producers_policy") {
		t.Fatalf("the opened document declares the PRODUCER's schema at a path the bundle ships itself:\n%s", rec.Body.String())
	}
	fi, ok := unitFileNamed(opened.Unit, "lib/app-policy.bot")
	if !ok {
		t.Fatalf("the unit does not name the file the bundle ships: %+v", opened.Unit.Files)
	}
	if fi.Dependency != "" {
		t.Fatalf("the bundle's own file is answered as bundle %q's, so the editor offers no edit of the one file the author has to drop", fi.Dependency)
	}
	// The write gate still refuses the bundle: the open is a way IN, never a
	// blessing.
	if rec := putFragmentBundle(t, s, editor, "consumer", files); rec.Code == http.StatusOK {
		t.Fatalf("the gate stored a bundle the launch refuses (%v): %s", launch, rec.Body.String())
	}
}

// TestADeleteScopesAShadowRefusalByPathNotBySentence: the delete route scopes
// a resolution failure to what the delete INTRODUCED, and a no-shadow refusal
// met every declared entry — so a verdict read off its SENTENCE reads the walk
// order. With two shadows, the delete of either strictly removes one and
// introduces nothing; comparing messages refuses whichever one does not sort
// first, naming a file the request never touched, and takes the other. The
// comparison is by path, so both are taken — and a delete that adds a path to
// the set is still refused.
func TestADeleteScopesAShadowRefusalByPathNotBySentence(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentProducerBoth(t, root)
	s.cfg.Bots.Paths = []string{root}
	ctx := store.WithTenant(context.Background(), "t1")

	// Both declared paths shipped, no record: two shadows. Stored past the
	// gate the way a push from before this rule left it.
	twoShadows := fragmentConsumerDeclaringBoth(map[string]string{
		"lib/forge-auth.bot": sharedFragmentBody,
		"lib/app-policy.bot": producersPolicyBody,
	})
	// BOTH orders, on identical bundles: a witness that can only redden for
	// one of the two is reading the sort order as hard as the code was.
	for i, rel := range []string{"lib/app-policy.bot", "lib/forge-auth.bot"} {
		slug := fmt.Sprintf("two%d", i)
		if _, err := s.botSources.Create(ctx, botsource.BotSource{TenantID: "t1", Slug: slug, Files: twoShadows}); err != nil {
			t.Fatal(err)
		}
		if rec := deleteFragmentBundleFile(t, s, editor, slug, rel); rec.Code != http.StatusOK {
			t.Fatalf("the delete of %s removes one of two shadows and introduces nothing, and is refused: %d %s", rel, rec.Code, rec.Body.String())
		}
	}

	// The other direction, on a bundle that ALREADY fails: one shipped path
	// is recorded (an install's own output, no shadow) and the other is not.
	// Deleting the record ADDS the recorded path to the shadow set, so the
	// delete introduced a failure and is refused — which a rule reading only
	// "was there a failure before" would take.
	mixed := fragmentConsumerDeclaringBoth(map[string]string{
		"lib/forge-auth.bot":  sharedFragmentBody,
		"lib/app-policy.bot":  producersPolicyBody,
		bundle.LocalStateFile: bundle.SHA256Bytes([]byte(sharedFragmentBody)) + " lib/forge-auth.bot\n",
	})
	if _, err := s.botSources.Create(ctx, botsource.BotSource{TenantID: "t1", Slug: "mixed", Files: mixed}); err != nil {
		t.Fatal(err)
	}
	rec := deleteFragmentBundleFile(t, s, editor, "mixed", bundle.LocalStateFile)
	if rec.Code == http.StatusOK {
		t.Fatalf("the delete of the materialization record added a shadow and was taken: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "lib/forge-auth.bot") {
		t.Fatalf("the refusal does not name the path the delete turned into a shadow:\n%s", rec.Body.String())
	}
}
