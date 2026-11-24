package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// resolveSnapshotFragments materializes the `dependencies.fragments` one
// bundle of the frozen collection declares, INTO the collection, at the path
// its own `import "lib/<name>.bot"` already spells — ADR-145's resolution on
// the launch plane.
//
// Why here, and why no store. A consumer that declares a shared fragment stops
// carrying its bytes, and the two carriers that bring a bundle to the server
// plane both drop what an install materialized: PackDir leaves the local-state
// file and its materialized members out of a .botz ([bundle.LocalStateFile]),
// and a bundle authored in the studio never had them. Without this pass the
// freeze PARSES a consumer whose fragment is absent, so the launch dies on
// `E046 import "lib/x.bot": no such fragment` — a true sentence about a file
// the author never wrote, blaming a bundle that is perfectly valid git-native
// and naming neither the dependency, nor the producer, nor the way forward.
//
// Why it is called when a bundle JOINS the collection, not after the walk like
// the skill pass. A fragment is part of the compilation unit: the walk loads
// each workflow as a unit to discover its children, so a fragment must be in
// the collection before the unit that imports it is read. The skill twin can
// run after the walk because nothing parses a skill.
//
// The producer's authority is resolved HERE, through the same team-scoped
// tiers `snapshotLaunchBot` resolves a child workflow's authority with, and
// never on a pod. Nothing durable is created: the resolved fragment travels
// inside the snapshot the run already persists, content-addressed by
// [bundle.Snapshot.Encode]'s digest, and its bytes are recorded by the path
// that records every other file of the unit (`workflow_sources`), exactly as
// the copy it replaces was. A fragment store on the server would be the second
// source of truth ADR-145 forbids ("the server plane is a parallel mechanism,
// never the place a shared fragment's bytes come from when the tree has them").
func (s *Server) resolveSnapshotFragments(ctx context.Context, teamID string, snap *bundle.Snapshot, root string, producers *snapshotProducers) error {
	manifest, err := snapshotManifest(snap, root)
	if err != nil {
		return err
	}
	if manifest == nil || len(manifest.Dependencies.Fragments) == 0 {
		return nil
	}
	deps := append([]bundle.FragmentDependency(nil), manifest.Dependencies.Fragments...)
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	recorded, err := snapshotLocalStateRecord(snap, root, bundle.LocalStateFragment)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		rel := dep.FragmentRelPath()
		key := root + "/" + rel
		existing, present := snap.Files[key]
		current := ""
		if present {
			sum := sha256.Sum256(existing.Content)
			current = hex.EncodeToString(sum[:])
		}
		if err := botinstall.RefuseFragmentShadow(rel, current, present, dep, recorded); err != nil {
			return fmt.Errorf("bundle snapshot: bundle %q: %w", root, err)
		}
		content, err := s.exportedFragmentForSnapshot(ctx, teamID, root, dep, producers)
		if err != nil {
			return err
		}
		snap.Files[key] = bundle.SnapshotFile{Content: content}
	}
	return nil
}

// exportedFragmentForSnapshot resolves one dependency's fragment through the
// launch's own tiers. teamID is the tenant the launch is FOR, passed down
// rather than read off the ctx for the reason resolveBotSource states: a tier
// resolved off whichever tenant happened to be on the ctx answers a different
// question, and here it would compile one team's bot against another's logic.
//
// A tier that FAILS is transient and wrapped in errBotSnapshotResolve, which
// the resume path retries — a store blip must not turn a launchable bot into a
// permanently broken one. A tier that answers "nowhere" is a declaration error
// and permanent, and says which tiers were searched. `iterion bots sync` is
// NOT the way forward here, as it is at the install: a server has no working
// tree to sync, so what unblocks the launch is the producer reaching a tier.
func (s *Server) exportedFragmentForSnapshot(ctx context.Context, teamID, consumer string, dep bundle.FragmentDependency, producers *snapshotProducers) ([]byte, error) {
	dir, err := producers.dir(ctx, s, teamID, dep.From)
	if err != nil {
		// %w keeps the transient sentinel reachable through this sentence:
		// the resume path tests for it, and a store blip must re-arm rather
		// than retire a bot that launches.
		return nil, fmt.Errorf("resolve shared fragment %q of bundle %q: %w", dep.ExportedID(), consumer, err)
	}
	if dir == "" {
		return nil, fmt.Errorf("bundle snapshot: bundle %q declares shared fragment %q from bundle %q, which resolves in no tier of this launch (team, platform, baked catalog): publish %q or drop the dependency", consumer, dep.ExportedID(), dep.From, dep.From)
	}
	source, err := botinstall.ExportedFragmentSource(dir, dep)
	if errors.Is(err, botinstall.ErrNoProducerBundle) {
		return nil, fmt.Errorf("bundle snapshot: bundle %q declares shared fragment %q from bundle %q, which resolved to something that is not a readable bundle: %v", consumer, dep.ExportedID(), dep.From, err)
	}
	if err != nil {
		return nil, fmt.Errorf("bundle snapshot: bundle %q: %w", consumer, err)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("bundle snapshot: bundle %q: read shared fragment %q of bundle %q: %w", consumer, dep.ExportedID(), dep.From, err)
	}
	return content, nil
}
