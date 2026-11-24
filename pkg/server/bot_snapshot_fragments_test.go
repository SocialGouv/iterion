package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The shared fragment both planes must answer with, byte for byte. It holds a
// real declaration the consumer's workflow REFERENCES, so a consumer that did
// not reach it does not compile and no witness here can pass by accident.
const sharedFragmentBody = "schema shared_state:\n  ok: bool\n  fail_log: string\n"

// sharedFragmentDeclaration is the name sharedFragmentBody declares; the
// merged program carrying it is what proves the fragment arrived BEFORE the
// parse, not merely that a file was injected somewhere.
const sharedFragmentDeclaration = "shared_state"

const fragmentConsumerMain = "import \"lib/forge-auth.bot\"\n\ncompute gate:\n  output: shared_state\n  expr:\n    ok: \"true\"\n    fail_log: \"''\"\n\nworkflow consumer:\n  entry: gate\n"

const fragmentConsumerManifest = "name: consumer\nversion: 1.0.0\ndependencies:\n  fragments:\n    - name: forge-auth\n      from: producer\n"

const fragmentProducerManifest = "name: producer\nversion: 1.0.0\nexports:\n  fragments:\n    - id: forge-auth\n      path: lib/forge-auth.bot\n"

// writeFragmentBundles lays out the consumer (which DECLARES the fragment, so
// its `import` names a file it does not carry) and the producer (which exports
// it) as sibling bundle directories under root.
func writeFragmentBundles(t *testing.T, root string) {
	t.Helper()
	for dir, files := range map[string]map[string]string{
		"consumer": {bundle.ManifestFile: fragmentConsumerManifest, "main.bot": fragmentConsumerMain},
		"producer": {bundle.ManifestFile: fragmentProducerManifest, "main.bot": testBotMain, "lib/forge-auth.bot": sharedFragmentBody},
	} {
		for rel, body := range files {
			p := filepath.Join(root, dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// resolveSharedFragmentGitNative is the install plane: the producer sits
// beside the installed consumer, and botinstall.ResolveFragments materializes
// the fragment into the consumer's own lib/.
func resolveSharedFragmentGitNative(t *testing.T) []byte {
	t.Helper()
	workdir := t.TempDir()
	root := filepath.Join(workdir, ".botz")
	writeFragmentBundles(t, root)
	installed := filepath.Join(root, "consumer")
	if _, err := botinstall.ResolveFragments(workdir, installed); err != nil {
		t.Fatalf("git-native resolution: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(installed, "lib", "forge-auth.bot"))
	if err != nil {
		t.Fatalf("git-native plane left no fragment: %v", err)
	}
	return got
}

// resolveSharedFragmentInSnapshot is the cloud plane: the launch freezes the
// consumer's collection, resolves the producer through the launch's tiers and
// injects the fragment into the snapshot the run persists.
func resolveSharedFragmentInSnapshot(t *testing.T) []byte {
	t.Helper()
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentBundles(t, root)
	s.cfg.Bots.Paths = []string{root}
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err != nil {
		t.Fatalf("cloud resolution: %v", err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	file, ok := snap.Files["consumer/lib/forge-auth.bot"]
	if !ok {
		t.Fatalf("cloud plane froze a lib/ short by one fragment; files: %v", snapshotKeys(snap))
	}
	return file.Content
}

// Both planes resolve the SAME declaration, and the bytes must be the same
// bytes: the launch plane is a parallel mechanism over the same files
// (ADR-145), not a second source of truth that could answer differently.
func TestASharedFragmentResolvesByteIdenticallyOnBothPlanes(t *testing.T) {
	planes := []struct {
		name    string
		resolve func(*testing.T) []byte
	}{
		{"git-native install", resolveSharedFragmentGitNative},
		{"cloud launch snapshot", resolveSharedFragmentInSnapshot},
	}
	answers := make(map[string]string, len(planes))
	for _, plane := range planes {
		t.Run(plane.name, func(t *testing.T) {
			got := plane.resolve(t)
			if string(got) != sharedFragmentBody {
				t.Fatalf("%s resolved %q, want the producer's bytes %q", plane.name, got, sharedFragmentBody)
			}
			answers[plane.name] = string(got)
		})
	}
	if len(answers) == len(planes) && answers[planes[0].name] != answers[planes[1].name] {
		t.Fatalf("the two planes disagree: %q vs %q", answers[planes[0].name], answers[planes[1].name])
	}
}

// What a fragment asks of the freeze and a skill does not: the freeze PARSES
// the consumer, so the fragment must be in the collection before the unit that
// imports it is read. The witness is the unit the RUNNER gets — loaded from
// the materialized snapshot, with the shared declaration in its merged
// program and no diagnostic — because an injection that happened after the
// walk would leave the launch dead on an E046 about a file the author never
// wrote.
func TestTheFrozenConsumerCompilesWithTheFragmentItDoesNotCarry(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentBundles(t, root)
	s.cfg.Bots.Paths = []string{root}
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err != nil {
		t.Fatalf("cloud resolution of a consumer declaring a fragment: %v", err)
	}
	defer lb.Cleanup()
	u, err := unit.LoadDir(filepath.Join(lb.BundleDir, "main.bot"))
	if err != nil {
		t.Fatalf("the frozen consumer does not load: %v", err)
	}
	if len(u.Unread) != 0 || len(u.Diagnostics) != 0 {
		t.Fatalf("the frozen consumer: unread=%v diagnostics=%v, want neither", u.Unread, u.Diagnostics)
	}
	if u.Merged == nil {
		t.Fatal("the frozen consumer has no merged program")
	}
	found := false
	for _, schema := range u.Merged.Schemas {
		if schema.Name == sharedFragmentDeclaration {
			found = true
		}
	}
	if !found {
		t.Fatalf("the shared declaration %q is not in the frozen program", sharedFragmentDeclaration)
	}
}

// A producer another TEAM pushed is not this launch's producer. The tiers are
// team-scoped, so the refusal is the whole point: resolving the dependency off
// any tenant that happens to be reachable would compile one team's bot against
// another team's logic.
func TestASharedFragmentDoesNotCrossTeams(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentBundles(t, root)
	// The baked catalog serves the consumer alone: the producer exists only
	// as another team's stored bundle.
	if err := os.RemoveAll(filepath.Join(root, "producer")); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
	other := "t2"
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), other), botsource.BotSource{
		TenantID: other, Slug: "producer",
		Files: map[string]string{
			botsource.MainBotFile: testBotMain,
			bundle.ManifestFile:   fragmentProducerManifest,
			"lib/forge-auth.bot":  sharedFragmentBody,
		},
	}); err != nil {
		t.Fatalf("seed the other team's producer: %v", err)
	}
	// The ctx carries the OTHER team, the launch is for t1: a resolution that
	// read the tenant off the ctx instead of taking the launch's own would
	// hand t1 the bundle t2 pushed.
	ctx := store.WithTenant(context.Background(), other)
	lb, err := s.resolveBotSource(ctx, "t1", "consumer")
	if err == nil {
		lb.Cleanup()
		t.Fatal("a producer stored by another team resolved for this launch")
	}
	assertFragmentRefusalNamesTheActors(t, err)
}

// No tier provides the producer: the refusal names the consumer, the fragment,
// the producer and where it looked — BEFORE the parse blames a file. The
// sentence it replaces is the defect of #2853: a valid bundle read as a
// corrupt one, by an `E046 no such fragment` about a file the author never
// wrote.
func TestAnUnresolvableSharedFragmentStopsTheLaunchBeforeTheParseBlamesAFile(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentBundles(t, root)
	if err := os.RemoveAll(filepath.Join(root, "producer")); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err == nil {
		lb.Cleanup()
		t.Fatal("a consumer whose producer is nowhere launched anyway")
	}
	assertFragmentRefusalNamesTheActors(t, err)
}

// assertFragmentRefusalNamesTheActors holds both halves of the ticket's ask,
// and asks the WHICH-ACTOR half first: a refusal that still reads
// `no such fragment` points at a file the author never wrote instead of at
// the dependency, which is the defect itself, so it is what a regression
// should say out loud. The second half is that the sentence names the four
// actors — a terse refusal is a different defect, and has its own mutant.
func assertFragmentRefusalNamesTheActors(t *testing.T, err error) {
	t.Helper()
	for _, unwanted := range []string{"no such fragment", "E046"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Fatalf("the refusal still blames the consumer's own file (%q): %v", unwanted, err)
		}
	}
	for _, want := range []string{"consumer", "forge-auth", "producer", "no tier"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
}

// A fragment the bundle itself ships is not a dependency's to overwrite
// (ADR-145 clause 2), and the frozen record is what tells a previous
// resolution's own output apart from the producer's bytes — so a bundle
// launched straight from a synced working tree resolves instead of being
// refused.
func TestASnapshotRefusesADependencyThatShadowsAShippedFragment(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentBundles(t, root)
	shipped := filepath.Join(root, "consumer", "lib", "forge-auth.bot")
	if err := os.MkdirAll(filepath.Dir(shipped), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shipped, []byte("schema shared_state:\n  ok: bool\n  fail_log: string\n  mine: bool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err == nil {
		lb.Cleanup()
		t.Fatal("a dependency silently replaced a fragment the bundle ships")
	}
	if !strings.Contains(err.Error(), "would shadow") {
		t.Fatalf("shadow refusal expected, got: %v", err)
	}
	// With the bundle's own record describing that exact file, it is this
	// installer's output and the launch resolves it.
	digest, err := bundle.FileSHA256(shipped)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.WriteLocalStateOfKind(filepath.Join(root, "consumer"), bundle.LocalStateFragment,
		[]bundle.LocalStateEntry{{Path: "lib/forge-auth.bot", SHA256: digest}}); err != nil {
		t.Fatal(err)
	}
	lb, err = s.resolveBotSource(context.Background(), "t1", "consumer")
	if err != nil {
		t.Fatalf("a recorded materialization was refused as a shipped fragment: %v", err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(snap.Files["consumer/lib/forge-auth.bot"].Content); got != sharedFragmentBody {
		t.Fatalf("the recorded file was not re-resolved: %q", got)
	}
}

// A CHILD bundle that declares a shared fragment is not a lesser bundle: the
// freeze parses its workflow too, so a child short by one fragment kills the
// whole launch. The fix is at the class — every bundle of the frozen
// collection, the moment it joins — and this is the case the ORDER is hardest
// in: the child's directory is added in the middle of the walk.
func TestASharedFragmentIsResolvedForAChildBundleToo(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentBundles(t, root)
	// The consumer becomes a CHILD: a parent bundle launches it as a subbot.
	for rel, body := range map[string]string{
		bundle.ManifestFile: "name: parent\nversion: 1.0.0\n",
		"main.bot":          snapshotParent("consumer"),
	} {
		p := filepath.Join(root, "parent", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.Bots.Paths = []string{root}
	lb, err := s.resolveBotSource(context.Background(), "t1", "parent")
	if err != nil {
		t.Fatalf("cloud resolution of a parent whose child declares a fragment: %v", err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(snap.Files["consumer/lib/forge-auth.bot"].Content); got != sharedFragmentBody {
		t.Fatalf("the child's declared fragment was not resolved (%q); files: %v", got, snapshotKeys(snap))
	}
}

// ONE producer, both artefact kinds, one resolution. The cache belongs to the
// FREEZE and not to a pass: a cache per pass would resolve the same bundle
// twice for a consumer that declares a skill and a fragment from it, which is
// how a row changing mid-freeze lands bytes from two versions of one producer
// in a single run.
func TestOneProducerServesASkillAndAFragmentFromOneResolution(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentBundles(t, root)
	// The consumer declares BOTH kinds from the same producer.
	consumerManifest := fragmentConsumerManifest + "  skills:\n    - name: verify-build\n      from: producer\n"
	if err := os.WriteFile(filepath.Join(root, "consumer", bundle.ManifestFile), []byte(consumerManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// The producer lives in the TEAM tier, so every resolution is a store read.
	if err := os.RemoveAll(filepath.Join(root, "producer")); err != nil {
		t.Fatal(err)
	}
	counting := &countingProducerStore{Store: s.botSources, slug: "producer"}
	s.botSources = counting
	producerManifest := fragmentProducerManifest + "  skills:\n    - id: verify-build\n      path: skills/verify-build.md\n"
	if _, err := counting.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "producer",
		Files: map[string]string{
			botsource.MainBotFile:    testBotMain,
			bundle.ManifestFile:      producerManifest,
			"lib/forge-auth.bot":     sharedFragmentBody,
			"skills/verify-build.md": sharedSkillBody,
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
	before := counting.count()
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err != nil {
		t.Fatalf("cloud resolution of a skill and a fragment from one producer: %v", err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"consumer/lib/forge-auth.bot":     sharedFragmentBody,
		"consumer/skills/verify-build.md": sharedSkillBody,
	} {
		if got := string(snap.Files[rel].Content); got != want {
			t.Fatalf("%s froze %q, want %q", rel, got, want)
		}
	}
	if reads := counting.count() - before; reads != 1 {
		t.Fatalf("the producer was read %d times for one freeze, want 1", reads)
	}
}

// fragmentProducerOutageStore fails the read of the producer alone, the way
// snapshotFailChildStore fails a child's.
type fragmentProducerOutageStore struct{ botsource.Store }

func (s fragmentProducerOutageStore) GetBySlug(ctx context.Context, tenant, slug string) (botsource.BotSource, error) {
	if slug == "producer" {
		return botsource.BotSource{}, errors.New("temporary store outage")
	}
	return s.Store.GetBySlug(ctx, tenant, slug)
}

// A store blip while resolving the producer is TRANSIENT: the resume re-arms
// instead of retiring a bot that launches. A declaration error and an outage
// must not read the same, or one bad minute permanently breaks a good bundle.
func TestASharedFragmentOutageIsTransientForTheResume(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeFragmentBundles(t, root)
	s.cfg.Bots.Paths = []string{root}
	s.botSources = fragmentProducerOutageStore{Store: s.botSources}
	_, err := s.resolveResumeBot(context.Background(), "", filepath.Join(root, "consumer/main.bot"))
	if !errors.Is(err, errResumeResolveTransient) {
		t.Fatalf("a producer store outage abandoned the resume: %v", err)
	}
	// The resume wrapper flattens its cause into text (`%v`, as it does for a
	// child workflow's authority), so the sentinel a caller tests for is the
	// resume's own; what the sentence must still carry is WHICH dependency of
	// WHICH bundle could not be resolved.
	for _, want := range []string{"forge-auth", "consumer", "producer", "bundle snapshot authority unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the outage sentence does not name %q: %v", want, err)
		}
	}
}
