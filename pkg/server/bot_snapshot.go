package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/runview"
)

var errBotSnapshotResolve = errors.New("bundle snapshot authority unavailable")

// snapshotLaunchBot freezes all files before compilation and keeps the same
// collection for the runner. Child authority is resolved HERE, never on a pod.
func (s *Server) snapshotLaunchBot(ctx context.Context, teamID string, lb *launchBot, sourceOverride ...string) (_ *launchBot, err error) {
	defer func() {
		if err != nil {
			lb.Cleanup()
		}
	}()
	root := filepath.Base(filepath.Dir(lb.Path))
	snap := &bundle.Snapshot{Root: root}
	dir := lb.BundleDir
	if dir == "" {
		dir = filepath.Dir(lb.Path)
	}
	entry := root + "/" + filepath.Base(lb.Path)
	// The snapshot's required file is the bundle's ENTRY — the file the
	// launch names — not the literal main.bot (#1896). A loose catalogue
	// file (bots/smoke/board_smoke.bot: no main.bot beside it, so no
	// bundle's entry — DirForEntry answers "") is frozen ALONE with the
	// fragments its imports reach: the directory it sits in is no bundle,
	// and capturing it whole would hand the run whatever else it holds —
	// an .env, a devbox.json, an unrelated skills/ the runner would then
	// mirror. A promoted sibling entry (extend.bot beside a main.bot)
	// freezes its whole bundle, as any bundle's launch does.
	if base := filepath.Base(lb.Path); base != bundle.MainBotFile {
		snap.Entry = entry
		lb.BundleEntry = base
	}
	if snap.Entry != "" && !mainBotBeside(dir) {
		if err := snap.AddLooseEntry(root, lb.Path, unit.FragmentDir); err != nil {
			return nil, err
		}
	} else if err := snap.AddDir(root, dir); err != nil {
		return nil, err
	}
	if len(sourceOverride) > 0 && sourceOverride[0] != "" {
		file := snap.Files[entry]
		file.Content = []byte(sourceOverride[0])
		snap.Files[entry] = file
	}
	type pending struct {
		file  string
		depth int
	}
	queue := []pending{{entry, 0}}
	seen := map[string]bool{}
	// One producer is resolved ONCE per freeze, and every dependency on it —
	// of either artefact kind — reads that one materialization: resolving per
	// dependency would let two declarations naming the same bundle land bytes
	// from two versions of it when the row changes mid-freeze, and would write
	// the same bundle to the temp tree once per dependency.
	producers := newSnapshotProducers()
	defer producers.release()
	loaded := map[string]bool{}
	// A bundle brings the FRAGMENTS it declares the moment it joins the
	// collection, which is before any of its workflows is parsed: a fragment
	// is part of the compilation unit the parse below reads, where a skill is
	// read by nobody until the run starts (resolveSnapshotSkills, after the
	// walk).
	join := func(name string) error {
		loaded[name] = true
		return s.resolveSnapshotFragments(ctx, teamID, snap, name, producers)
	}
	if err := join(root); err != nil {
		return nil, err
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current.file] {
			continue
		}
		seen[current.file] = true
		if _, ok := snap.Files[current.file]; !ok {
			return nil, fmt.Errorf("bundle snapshot: child workflow %q is missing", current.file)
		}
		// The workflow's unit, read from the frozen collection: its
		// fragments sit beside it under the same root, and a child a
		// fragment declares is a child of this workflow — made relative to
		// the root on the merged copy, the base every host resolves it from.
		dir := path.Dir(current.file)
		u, err := unit.Load(func(rel string) ([]byte, error) {
			f, ok := snap.Files[path.Join(dir, rel)]
			if !ok {
				return nil, fmt.Errorf("%w: %s", os.ErrNotExist, rel)
			}
			return f.Content, nil
		}, path.Base(current.file), func(rel string) string { return path.Join(dir, rel) })
		if err != nil {
			return nil, fmt.Errorf("bundle snapshot: parse %s: %w", current.file, err)
		}
		for _, diagnostic := range u.Diagnostics {
			if diagnostic.Severity == parser.SeverityError {
				return nil, fmt.Errorf("bundle snapshot: parse %s: %s", current.file, diagnostic.Error())
			}
		}
		if u.Merged == nil {
			return nil, fmt.Errorf("bundle snapshot: no AST for %s", current.file)
		}
		for _, child := range u.Merged.Subbots {
			if current.depth >= 8 {
				return nil, fmt.Errorf("bundle snapshot: child depth exceeds 8 at %s", current.file)
			}
			if path.IsAbs(child.Source) || strings.ContainsAny(child.Source, "\\\x00") {
				return nil, fmt.Errorf("bundle snapshot: unsafe subbot source %q", child.Source)
			}
			key := path.Clean(path.Join(path.Dir(current.file), child.Source))
			parts := strings.SplitN(key, "/", 2)
			if len(parts) != 2 || parts[0] == ".." || parts[0] == "." {
				return nil, fmt.Errorf("bundle snapshot: subbot %q escapes the collection", child.Source)
			}
			if !loaded[parts[0]] {
				resolved, err := s.resolveBotTieredRaw(ctx, teamID, parts[0], "")
				if err != nil {
					return nil, fmt.Errorf("%w: resolve child %s: %v", errBotSnapshotResolve, parts[0], err)
				}
				if resolved == nil {
					return nil, fmt.Errorf("bundle snapshot: child bundle %q not found", parts[0])
				}
				childDir := resolved.BundleDir
				if childDir == "" {
					childDir = filepath.Dir(resolved.Path)
				}
				err = snap.AddDir(parts[0], childDir)
				resolved.Cleanup()
				if err != nil {
					return nil, err
				}
				if err := join(parts[0]); err != nil {
					return nil, err
				}
			}
			queue = append(queue, pending{key, current.depth + 1})
		}
	}
	// Every bundle of the collection is frozen now, so a dependency declared
	// by the launched bundle OR by a child resolves against the same loaded
	// set — and a child that declares one is not a lesser bundle (ADR-144's
	// twin, ARB-114 = A).
	rootNames := make([]string, 0, len(loaded))
	for name := range loaded {
		rootNames = append(rootNames, name)
	}
	if err := s.resolveSnapshotSkills(ctx, teamID, snap, rootNames, producers); err != nil {
		return nil, err
	}
	body, digest, err := snap.Encode()
	if err != nil {
		return nil, err
	}
	frozenDir, cleanup, err := snap.Materialize()
	if err != nil {
		return nil, err
	}
	lb.Cleanup()
	lb.BundleDir, lb.cleanupSnapshot = frozenDir, cleanup
	lb.Source = string(snap.Files[entry].Content)
	if lb.Ref == nil {
		lb.Ref = &runview.BotBundleRef{Slug: lb.BotID}
	}
	lb.Ref.Snapshot, lb.Ref.SnapshotDigest = body, digest
	return lb, nil
}

// mainBotBeside reports whether dir holds a regular main.bot — the fact
// that makes a sibling .bot a bundle's promoted entry (its bundle is
// frozen whole) rather than a loose catalogue file (frozen alone).
func mainBotBeside(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, bundle.MainBotFile))
	return err == nil && info.Mode().IsRegular()
}
