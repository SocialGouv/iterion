package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The shared skill both planes must answer with, byte for byte. Its
// frontmatter name agrees with the name the consumer references it under,
// which is what keeps the resolution from refusing a renamed document.
const sharedSkillBody = "---\nname: verify-build\ndescription: build it\n---\n\nRun the build, then read the log.\n"

const skillConsumerManifest = "name: consumer\nversion: 1.0.0\ndependencies:\n  skills:\n    - name: verify-build\n      from: producer\n"

const skillProducerManifest = "name: producer\nversion: 1.0.0\nexports:\n  skills:\n    - id: verify-build\n      path: skills/verify-build.md\n"

// writeSkillBundles lays out the consumer (which DECLARES the skill and does
// not carry it) and the producer (which exports it) as sibling bundle
// directories under root.
func writeSkillBundles(t *testing.T, root string) {
	t.Helper()
	for dir, files := range map[string]map[string]string{
		"consumer": {bundle.ManifestFile: skillConsumerManifest, "main.bot": testBotMain},
		"producer": {bundle.ManifestFile: skillProducerManifest, "main.bot": testBotMain, "skills/verify-build.md": sharedSkillBody},
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

// resolveSharedSkillGitNative is the install plane: the producer sits beside
// the installed consumer, and botinstall.ResolveSkills materializes the
// document into the consumer's own skills directory.
func resolveSharedSkillGitNative(t *testing.T) []byte {
	t.Helper()
	workdir := t.TempDir()
	root := filepath.Join(workdir, ".botz")
	writeSkillBundles(t, root)
	installed := filepath.Join(root, "consumer")
	if _, err := botinstall.ResolveSkills(workdir, installed); err != nil {
		t.Fatalf("git-native resolution: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(installed, "skills", "verify-build.md"))
	if err != nil {
		t.Fatalf("git-native plane left no document: %v", err)
	}
	return got
}

// resolveSharedSkillInSnapshot is the cloud plane: the launch freezes the
// consumer's collection, resolves the producer through the launch's tiers and
// injects the document into the snapshot the run persists.
func resolveSharedSkillInSnapshot(t *testing.T) []byte {
	t.Helper()
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeSkillBundles(t, root)
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
	file, ok := snap.Files["consumer/skills/verify-build.md"]
	if !ok {
		t.Fatalf("cloud plane froze a skills directory short by one file; files: %v", snapshotKeys(snap))
	}
	return file.Content
}

// Both planes resolve the SAME declaration, and the bytes must be the same
// bytes: the cloud twin is a parallel mechanism over the same documents
// (ADR-144), not a second source of truth that could answer differently.
func TestASharedSkillResolvesByteIdenticallyOnBothPlanes(t *testing.T) {
	planes := []struct {
		name    string
		resolve func(*testing.T) []byte
	}{
		{"git-native install", resolveSharedSkillGitNative},
		{"cloud launch snapshot", resolveSharedSkillInSnapshot},
	}
	answers := make(map[string]string, len(planes))
	for _, plane := range planes {
		t.Run(plane.name, func(t *testing.T) {
			got := plane.resolve(t)
			if string(got) != sharedSkillBody {
				t.Fatalf("%s resolved %q, want the producer's bytes %q", plane.name, got, sharedSkillBody)
			}
			answers[plane.name] = string(got)
		})
	}
	if len(answers) == len(planes) && answers[planes[0].name] != answers[planes[1].name] {
		t.Fatalf("the two planes disagree: %q vs %q", answers[planes[0].name], answers[planes[1].name])
	}
}

// A producer another TEAM pushed is not this launch's producer. The tiers are
// team-scoped, so the refusal is the whole point: resolving the dependency
// off any tenant that happens to be reachable would serve one team the
// playbook of another.
func TestASharedSkillDoesNotCrossTeams(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeSkillBundles(t, root)
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
			botsource.MainBotFile:    testBotMain,
			bundle.ManifestFile:      skillProducerManifest,
			"skills/verify-build.md": sharedSkillBody,
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
	for _, want := range []string{"consumer", "verify-build", "producer", "no tier"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("cross-team refusal does not name %q: %v", want, err)
		}
	}
}

// No tier provides the producer: the refusal names the consumer, the skill,
// the producer and where it looked — never a silently short skills directory,
// which is the failure ADR-144 clause 4 exists to end and the one a bot
// answers by getting quietly dumber.
func TestAnUnresolvableSharedSkillStopsTheLaunch(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeSkillBundles(t, root)
	if err := os.RemoveAll(filepath.Join(root, "producer")); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err == nil {
		lb.Cleanup()
		t.Fatal("a consumer whose producer is nowhere launched anyway")
	}
	for _, want := range []string{"consumer", "verify-build", "producer", "no tier"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
}

// A document the bundle itself ships is not a dependency's to overwrite
// (clause 5), and the frozen record is what tells a previous resolution's own
// output apart from the producer's bytes — so a bundle launched straight from
// a synced working tree resolves instead of being refused.
func TestASnapshotRefusesADependencyThatShadowsAShippedSkill(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeSkillBundles(t, root)
	shipped := filepath.Join(root, "consumer", "skills", "verify-build.md")
	if err := os.MkdirAll(filepath.Dir(shipped), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shipped, []byte("---\nname: verify-build\n---\n\nthe consumer's own\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err == nil {
		lb.Cleanup()
		t.Fatal("a dependency silently replaced a skill the bundle ships")
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
	if err := bundle.WriteLocalStateOfKind(filepath.Join(root, "consumer"), bundle.LocalStateSkill,
		[]bundle.LocalStateEntry{{Path: "skills/verify-build.md", SHA256: digest}}); err != nil {
		t.Fatal(err)
	}
	lb, err = s.resolveBotSource(context.Background(), "t1", "consumer")
	if err != nil {
		t.Fatalf("a recorded materialization was refused as a shipped skill: %v", err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(snap.Files["consumer/skills/verify-build.md"].Content); got != sharedSkillBody {
		t.Fatalf("the recorded file was not re-resolved: %q", got)
	}
}

// A bundle may spell its manifest `manifest.yml`, which every reader in
// pkg/bundle accepts: a freeze that read one spelling only would skip the
// resolution of the other and hand the run a short skills directory — the
// silent miss, reintroduced by a filename.
func TestASharedSkillResolvesForTheAltManifestSpelling(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeSkillBundles(t, root)
	if err := os.Rename(filepath.Join(root, "consumer", bundle.ManifestFile),
		filepath.Join(root, "consumer", bundle.ManifestFileAlt)); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err != nil {
		t.Fatalf("cloud resolution of a %s consumer: %v", bundle.ManifestFileAlt, err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(snap.Files["consumer/skills/verify-build.md"].Content); got != sharedSkillBody {
		t.Fatalf("a %s consumer froze %q, want the producer's bytes", bundle.ManifestFileAlt, got)
	}
}

// A CHILD bundle that declares a shared skill is not a lesser bundle: the
// run executes its workflow too, and a skills directory short by one file
// makes the child answer from priors exactly as the parent would. The fix is
// at the class — every bundle of the frozen collection — not at the launched
// one.
func TestASharedSkillIsResolvedForAChildBundleToo(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeSkillBundles(t, root)
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
		t.Fatalf("cloud resolution of a parent whose child declares a skill: %v", err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(snap.Files["consumer/skills/verify-build.md"].Content); got != sharedSkillBody {
		t.Fatalf("the child's declared skill was not resolved (%q); files: %v", got, snapshotKeys(snap))
	}
}

// countingProducerStore counts the reads of one slug, under the mutex the
// -race job asks of any observer a server call feeds.
type countingProducerStore struct {
	botsource.Store
	mu    sync.Mutex
	slug  string
	reads int
}

func (c *countingProducerStore) GetBySlug(ctx context.Context, tenant, slug string) (botsource.BotSource, error) {
	if slug == c.slug {
		c.mu.Lock()
		c.reads++
		c.mu.Unlock()
	}
	return c.Store.GetBySlug(ctx, tenant, slug)
}

func (c *countingProducerStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// Two dependencies on the SAME producer resolve from ONE materialization of
// it. Resolving per dependency would let a row that changes mid-freeze land
// two versions of one bundle in a single run, and would write the producer to
// the temp tree once per dependency.
func TestOneProducerIsResolvedOncePerFreeze(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeSkillBundles(t, root)
	// The consumer declares BOTH skills the producer exports.
	consumerManifest := skillConsumerManifest + "    - name: read-logs\n      from: producer\n"
	if err := os.WriteFile(filepath.Join(root, "consumer", bundle.ManifestFile), []byte(consumerManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// The producer lives in the TEAM tier, so every resolution is a store read.
	if err := os.RemoveAll(filepath.Join(root, "producer")); err != nil {
		t.Fatal(err)
	}
	counting := &countingProducerStore{Store: s.botSources, slug: "producer"}
	s.botSources = counting
	producerManifest := skillProducerManifest + "    - id: read-logs\n      path: skills/read-logs.md\n"
	if _, err := counting.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "producer",
		Files: map[string]string{
			botsource.MainBotFile:    testBotMain,
			bundle.ManifestFile:      producerManifest,
			"skills/verify-build.md": sharedSkillBody,
			"skills/read-logs.md":    "---\nname: read-logs\n---\n\nRead the log.\n",
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
	before := counting.count()
	lb, err := s.resolveBotSource(context.Background(), "t1", "consumer")
	if err != nil {
		t.Fatalf("cloud resolution of two skills from one producer: %v", err)
	}
	defer lb.Cleanup()
	snap, err := bundle.DecodeSnapshot(lb.Ref.Snapshot, lb.Ref.SnapshotDigest)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"consumer/skills/verify-build.md": sharedSkillBody,
		"consumer/skills/read-logs.md":    "---\nname: read-logs\n---\n\nRead the log.\n",
	} {
		if got := string(snap.Files[rel].Content); got != want {
			t.Fatalf("%s froze %q, want %q", rel, got, want)
		}
	}
	if reads := counting.count() - before; reads != 1 {
		t.Fatalf("the producer was read %d times for one freeze, want 1", reads)
	}
}

// skillProducerOutageStore fails the read of the producer alone, the way
// snapshotFailChildStore fails a child's.
type skillProducerOutageStore struct{ botsource.Store }

func (s skillProducerOutageStore) GetBySlug(ctx context.Context, tenant, slug string) (botsource.BotSource, error) {
	if slug == "producer" {
		return botsource.BotSource{}, errors.New("temporary store outage")
	}
	return s.Store.GetBySlug(ctx, tenant, slug)
}

// A store blip while resolving the producer is TRANSIENT: the resume re-arms
// instead of retiring a bot that launches. A declaration error and an outage
// must not read the same, or one bad minute permanently breaks a good bundle.
func TestASharedSkillOutageIsTransientForTheResume(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	root := filepath.Join(t.TempDir(), "bots")
	writeSkillBundles(t, root)
	s.cfg.Bots.Paths = []string{root}
	s.botSources = skillProducerOutageStore{Store: s.botSources}
	_, err := s.resolveResumeBot(context.Background(), "", filepath.Join(root, "consumer/main.bot"))
	if !errors.Is(err, errResumeResolveTransient) {
		t.Fatalf("a producer store outage abandoned the resume: %v", err)
	}
	// The resume wrapper flattens its cause into text (`%v`, as it does for a
	// child workflow's authority), so the sentinel a caller tests for is the
	// resume's own; what the sentence must still carry is WHICH dependency of
	// WHICH bundle could not be resolved.
	for _, want := range []string{"verify-build", "consumer", "producer", "bundle snapshot authority unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the outage sentence does not name %q: %v", want, err)
		}
	}
}
