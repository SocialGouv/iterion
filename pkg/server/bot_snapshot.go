package server

import (
	"context"
	"errors"
	"fmt"
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
	if err := snap.AddDir(root, dir); err != nil {
		return nil, err
	}
	entry := root + "/" + filepath.Base(lb.Path)
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
	loaded := map[string]bool{root: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current.file] {
			continue
		}
		seen[current.file] = true
		file, ok := snap.Files[current.file]
		if !ok {
			return nil, fmt.Errorf("bundle snapshot: child workflow %q is missing", current.file)
		}
		parsed := parser.Parse(current.file, string(file.Content))
		for _, diagnostic := range parsed.Diagnostics {
			if diagnostic.Severity == parser.SeverityError {
				return nil, fmt.Errorf("bundle snapshot: parse %s: %s", current.file, diagnostic.Error())
			}
		}
		if parsed.File == nil {
			return nil, fmt.Errorf("bundle snapshot: no AST for %s", current.file)
		}
		for _, child := range parsed.File.Subbots {
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
				loaded[parts[0]] = true
			}
			queue = append(queue, pending{key, current.depth + 1})
		}
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
