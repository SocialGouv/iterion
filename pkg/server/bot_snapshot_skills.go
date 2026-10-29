package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// resolveSnapshotSkills materializes the `dependencies.skills` every bundle of
// the frozen collection declares, INTO the collection — ADR-144's cloud twin,
// whose shape was decided as ARB-114 = A.
//
// Why here, and why no store. A consumer that declares a shared skill stops
// carrying its bytes (ADR-144 clause 1), and the two carriers that bring a
// bundle to the server plane both drop what an install materialized: PackDir
// leaves the local-state file and its materialized members out of a .botz
// ([bundle.LocalStateFile]), and a bundle authored in the studio never had
// them. So without this pass the launch freezes a skills directory that is
// short by one file, the run-start mirror mirrors it short, and the bot
// answers from the model's own priors — the silent failure clause 4 refuses,
// reproduced on the plane the git-native install never runs on.
//
// The producer's authority is resolved HERE, through the same team-scoped
// tiers `snapshotLaunchBot` resolves a child workflow's authority with, and
// never on a pod. Nothing durable is created: the resolved document travels
// inside the snapshot the run already persists, content-addressed by
// [bundle.Snapshot.Encode]'s digest — the digest IS the pin, which is what
// answers "which version of the shared logic did this run execute" at a tier
// that has no `bots.lock`. A skills collection in the store would be the
// second source of truth ADR-144 forbids ("the server plane is a parallel
// mechanism, never the place a shared skill's bytes come from when the tree
// has them").
//
// producers is the freeze's own cache, passed in rather than built here: a
// bundle that declares both a skill and a fragment from the same producer must
// read ONE materialization of it, which a cache per pass cannot promise.
func (s *Server) resolveSnapshotSkills(ctx context.Context, teamID string, snap *bundle.Snapshot, roots []string, producers *snapshotProducers) error {
	ordered := append([]string(nil), roots...)
	sort.Strings(ordered)
	for _, root := range ordered {
		manifest, err := snapshotManifest(snap, root)
		if err != nil {
			return err
		}
		if manifest == nil || len(manifest.Dependencies.Skills) == 0 {
			continue
		}
		deps := append([]bundle.SkillDependency(nil), manifest.Dependencies.Skills...)
		sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
		recorded, err := snapshotLocalStateRecord(snap, root, bundle.LocalStateSkill)
		if err != nil {
			return err
		}
		for _, dep := range deps {
			rel := dep.SkillRelPath()
			key := root + "/" + rel
			existing, present := snap.Files[key]
			current := ""
			if present {
				sum := sha256.Sum256(existing.Content)
				current = hex.EncodeToString(sum[:])
			}
			if err := botinstall.RefuseSkillShadow(rel, current, present, dep, recorded); err != nil {
				return fmt.Errorf("bundle snapshot: bundle %q: %w", root, err)
			}
			content, err := s.exportedSkillForSnapshot(ctx, teamID, root, dep, producers)
			if err != nil {
				return err
			}
			snap.Files[key] = bundle.SnapshotFile{Content: content}
		}
	}
	return nil
}

// snapshotManifest decodes a frozen bundle's manifest from the BYTES that
// will run, not from the directory they came from: between a resolution and a
// launch the directory is a temporary copy nobody else owns, and the frozen
// text is the one the digest covers.
//
// Both spellings, for the reason botsource.BotSource.Manifest states — one
// place decides "which manifest file, decoded how". A bundle with no manifest
// declares no dependency; a manifest that does not decode is an error here
// because every carrier that can reach this point already refused it (the
// botsource write path decodes both spellings, and the repository's own
// bundles are held by `task check`), so tolerating it would hide a carrier
// that stopped checking.
func snapshotManifest(snap *bundle.Snapshot, root string) (*bundle.Manifest, error) {
	for _, name := range []string{bundle.ManifestFile, bundle.ManifestFileAlt} {
		file, ok := snap.Files[root+"/"+name]
		if !ok {
			continue
		}
		manifest, err := bundle.DecodeManifest(file.Content, root+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("bundle snapshot: bundle %q: %s: %w", root, name, err)
		}
		return manifest, nil
	}
	return nil, nil
}

// snapshotLocalStateRecord reads the bundle's own record of what a previous
// resolution materialized for one KIND of local state, from the frozen copy:
// Snapshot.AddDir carries the live files of a directory, so a bundle launched
// straight from a synced working tree arrives WITH its materialized skill or
// fragment and the record that describes it. Reading the record is what tells
// that file apart from one the producer ships — the same question, the same
// answer, on both planes and for both artefact kinds.
func snapshotLocalStateRecord(snap *bundle.Snapshot, root string, kind bundle.LocalStateKind) (map[string]string, error) {
	file, ok := snap.Files[root+"/"+bundle.LocalStateFile]
	if !ok {
		return nil, nil
	}
	entries, err := bundle.DecodeLocalStateOfKind(file.Content, kind)
	if err != nil {
		return nil, fmt.Errorf("bundle snapshot: bundle %q: %w", root, err)
	}
	recorded := make(map[string]string, len(entries))
	for _, entry := range entries {
		recorded[entry.Path] = entry.SHA256
	}
	return recorded, nil
}

// exportedSkillForSnapshot resolves one dependency's document through the
// launch's own tiers. teamID is the tenant the launch is FOR, passed down
// rather than read off the ctx for the reason resolveBotSource states: a tier
// resolved off whichever tenant happened to be on the ctx answers a different
// question, and here it would serve one team the playbook of another.
//
// A tier that FAILS is transient and wrapped in errBotSnapshotResolve, which
// the resume path retries — a store blip must not turn a launchable bot into
// a permanently broken one. A tier that answers "nowhere" is a declaration
// error and permanent, and says which tiers were searched.
func (s *Server) exportedSkillForSnapshot(ctx context.Context, teamID, consumer string, dep bundle.SkillDependency, producers *snapshotProducers) ([]byte, error) {
	dir, err := producers.dir(ctx, s, teamID, dep.From)
	if err != nil {
		// %w keeps the transient sentinel reachable through this sentence:
		// the resume path tests for it, and a store blip must re-arm rather
		// than retire a bot that launches.
		return nil, fmt.Errorf("resolve shared skill %q of bundle %q: %w", dep.ExportedID(), consumer, err)
	}
	if dir == "" {
		return nil, fmt.Errorf("bundle snapshot: bundle %q declares shared skill %q from bundle %q, which resolves in no tier of this launch (team, platform, baked catalog): publish %q or drop the dependency", consumer, dep.ExportedID(), dep.From, dep.From)
	}
	source, err := botinstall.ExportedSkillSource(dir, dep)
	if errors.Is(err, botinstall.ErrNoProducerBundle) {
		return nil, fmt.Errorf("bundle snapshot: bundle %q declares shared skill %q from bundle %q, which resolved to something that is not a readable bundle: %v", consumer, dep.ExportedID(), dep.From, err)
	}
	if err != nil {
		return nil, fmt.Errorf("bundle snapshot: bundle %q: %w", consumer, err)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("bundle snapshot: bundle %q: read shared skill %q of bundle %q: %w", consumer, dep.ExportedID(), dep.From, err)
	}
	return content, nil
}

// snapshotProducers holds each producer bundle a freeze resolved, so the
// identity of "bundle X" is fixed once for the whole freeze rather than
// re-asked per dependency. A bundle that resolves NOWHERE is remembered as
// the empty directory: a second dependency on it must read the same answer,
// not a second store round trip whose outcome could differ.
type snapshotProducers struct {
	dirs     map[string]string
	cleanups []func()
}

func newSnapshotProducers() *snapshotProducers {
	return &snapshotProducers{dirs: map[string]string{}}
}

// dir resolves one producer through the launch's own tiers, once. A tier
// FAILURE is transient (errBotSnapshotResolve, which the resume retries) and
// is not remembered: the next freeze must be free to succeed.
func (p *snapshotProducers) dir(ctx context.Context, s *Server, teamID, from string) (string, error) {
	if dir, ok := p.dirs[from]; ok {
		return dir, nil
	}
	resolved, err := s.resolveBotTieredRaw(ctx, teamID, from, "")
	if err != nil {
		return "", fmt.Errorf("%w: resolve bundle %s: %v", errBotSnapshotResolve, from, err)
	}
	if resolved == nil {
		p.dirs[from] = ""
		return "", nil
	}
	p.cleanups = append(p.cleanups, resolved.Cleanup)
	dir := resolved.BundleDir
	if dir == "" {
		dir = filepath.Dir(resolved.Path)
	}
	p.dirs[from] = dir
	return dir, nil
}

// release drops every materialization the freeze owns. The snapshot holds the
// resolved bytes by then, so nothing below these directories is read again.
func (p *snapshotProducers) release() {
	for _, cleanup := range p.cleanups {
		cleanup()
	}
}
