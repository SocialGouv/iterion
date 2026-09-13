package runtime

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"golang.org/x/sync/semaphore"
)

// Children share the parent's code workspace, but borrow its managed resource
// paths only for an active pass. Readers of that workspace wait while a child
// owns it. A separate gate inside each child lets nested children take their
// own exclusive scope without deadlocking on an ancestor's lease.
const resourceWriterWeight int64 = 1 << 30

type resourceGate struct {
	sem  *semaphore.Weighted
	refs int
}

var workspaceResourceGates = struct {
	sync.Mutex
	byPath map[string]*resourceGate
	active map[resourceOwnerKey]*runResourceScope
}{byPath: map[string]*resourceGate{}, active: map[resourceOwnerKey]*runResourceScope{}}

type resourceOwnerKey struct{ path, runID string }

type resourceScopeKey struct{}
type runResourceScope struct {
	path         string
	runID        string
	borrowed     bool
	gate         *resourceGate
	owned        map[string]string // exact bytes last mirrored by the active parent
	setupRelease func()
	finish       func() error
}

func retainResourceGate(path string) (*resourceGate, func()) {
	workspaceResourceGates.Lock()
	defer workspaceResourceGates.Unlock()
	g := workspaceResourceGates.byPath[path]
	if g == nil {
		g = &resourceGate{sem: semaphore.NewWeighted(resourceWriterWeight)}
		workspaceResourceGates.byPath[path] = g
	}
	g.refs++
	return g, func() {
		workspaceResourceGates.Lock()
		defer workspaceResourceGates.Unlock()
		g.refs--
		if g.refs == 0 {
			delete(workspaceResourceGates.byPath, path)
		}
	}
}

// A resumed grandchild has a fresh context but its parent may still be parked
// in AwaitSubbotTerminal. Find that live scope rather than waiting on the
// ancestor's outer writer. Recheck after acquisition in case it closed meanwhile.
func acquireResourceWriter(ctx context.Context, dir, parentID string, fallback *resourceGate) (*resourceGate, *runResourceScope, error) {
	key := resourceOwnerKey{dir, parentID}
	for {
		workspaceResourceGates.Lock()
		parent := workspaceResourceGates.active[key]
		workspaceResourceGates.Unlock()
		gate := fallback
		if parent != nil {
			gate = parent.gate
		}
		if err := gate.sem.Acquire(ctx, resourceWriterWeight); err != nil {
			return nil, nil, err
		}
		workspaceResourceGates.Lock()
		current := workspaceResourceGates.active[key]
		workspaceResourceGates.Unlock()
		if current == parent {
			return gate, parent, nil
		}
		gate.sem.Release(resourceWriterWeight)
	}
}

func (e *Engine) beginRunResources(ctx context.Context, runID string, childInPlace bool) (context.Context, func() error, error) {
	e.resourceScope = nil
	dir, err := filepath.Abs(e.workDir)
	if err != nil {
		return ctx, nil, err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	parent, _ := ctx.Value(resourceScopeKey{}).(*runResourceScope)
	// Unrelated resource-free engines keep their existing execution path.
	needed := e.bundle != nil || e.contributions != nil || childInPlace || parent != nil
	if !needed {
		for _, node := range e.workflow.Nodes {
			if _, ok := node.(*ir.SubbotNode); ok {
				needed = true
				break
			}
		}
	}
	if !needed {
		return ctx, func() error { return nil }, nil
	}
	gate, release := retainResourceGate(dir)
	gate, parent, err = acquireResourceWriter(ctx, dir, e.parentRunID, gate)
	if err != nil {
		release()
		return ctx, nil, err
	}
	releaseWriter := sync.OnceFunc(func() { gate.sem.Release(resourceWriterWeight) })
	scope := &runResourceScope{path: dir, runID: runID, borrowed: childInPlace, gate: gate, owned: map[string]string{}}
	if parent != nil {
		for p, h := range parent.owned {
			scope.owned[p] = h
		}
	}
	scope.setupRelease = releaseWriter
	restore := func() error { return nil }
	if childInPlace {
		// Nodes within the child may run concurrently. Sibling/parent readers use
		// the outer gate, held exclusively until restoration has finished.
		scope.gate = &resourceGate{sem: semaphore.NewWeighted(resourceWriterWeight)}
		scope.setupRelease = func() {}
		restore, err = snapshotChildResources(dir)
		if err == nil {
			remoteRestore, remoteErr := e.snapshotSharedChildResources(ctx, "iterion-child-resources-"+rand.Text())
			if remoteErr != nil {
				err = remoteErr
			} else {
				restore = combineResourceRestore(remoteRestore, restore)
			}
		}
		if err == nil {
			owner := parent
			if owner == nil || owner.path != dir {
				owner = e.persistedParentResources(ctx, dir)
			}
			if owner != nil {
				err = e.hideParentSkillCollisions(owner)
			}
		}
		if err != nil {
			if restore != nil {
				err = errors.Join(err, restore())
			}
			releaseWriter()
			release()
			return ctx, nil, err
		}
	}
	scope.finish = func() error {
		workspaceResourceGates.Lock()
		delete(workspaceResourceGates.active, resourceOwnerKey{dir, runID})
		workspaceResourceGates.Unlock()
		scope.setupRelease()
		// Drain a separately resumed descendant before restoring its ancestors.
		_ = scope.gate.sem.Acquire(context.Background(), resourceWriterWeight)
		err := restore()
		scope.gate.sem.Release(resourceWriterWeight)
		releaseWriter()
		release()
		return err
	}
	e.resourceScope = scope
	return context.WithValue(ctx, resourceScopeKey{}, scope), scope.finish, nil
}

func (e *Engine) resourcesReady() {
	if e.resourceScope != nil {
		workspaceResourceGates.Lock()
		workspaceResourceGates.active[resourceOwnerKey{e.resourceScope.path, e.resourceScope.runID}] = e.resourceScope
		workspaceResourceGates.Unlock()
		e.resourceScope.setupRelease()
	}
}

// Every model/tool execution path shares this reader boundary; a subbot itself
// never holds a reader while waiting for its child to acquire the writer.
func (e *Engine) executeWithResources(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	scope := e.resourceScope
	if scope == nil {
		return e.executor.Execute(ctx, node, input)
	}
	if err := scope.gate.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer scope.gate.sem.Release(1)
	return e.executor.Execute(ctx, node, input)
}

var childResourcePaths = []string{"skills", "commands", "agents", "settings.json"}

// Copy only the directories the resource mirrors own. Other workspace edits,
// including code produced by the child, are deliberately outside this scope.
// Backups survive a failed restore, and the returned error names their path.
func snapshotChildResources(workDir string) (func() error, error) {
	backup, err := os.MkdirTemp("", "iterion-child-resources-")
	if err != nil {
		return nil, err
	}
	root := filepath.Join(workDir, ".claude")
	existed := map[string]bool{}
	for _, name := range childResourcePaths {
		src := filepath.Join(root, name)
		_, err := os.Lstat(src)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			err = copyResourceEntry(src, filepath.Join(backup, name))
		}
		if err != nil {
			_ = os.RemoveAll(backup)
			return nil, err
		}
		existed[name] = true
	}
	return func() error {
		for _, name := range childResourcePaths {
			dst := filepath.Join(root, name)
			if err := os.RemoveAll(dst); err != nil {
				return fmt.Errorf("restore child resources (backup %s): %w", backup, err)
			}
			if existed[name] {
				if err := copyResourceEntry(filepath.Join(backup, name), dst); err != nil {
					return fmt.Errorf("restore child resources (backup %s): %w", backup, err)
				}
			}
		}
		return os.RemoveAll(backup)
	}, nil
}

func copyResourceEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyResourceEntry(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(dst, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("resource %s is not a file, directory or symlink", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}

func (e *Engine) rememberResourceSkills(owned []string) {
	if e.resourceScope == nil {
		return
	}
	for _, p := range owned {
		_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if hash, err := hashFile(path); err == nil {
					e.resourceScope.owned[path] = hash
				}
			}
			return nil
		})
		// Flat bundle skills also have a path-based compatibility alias.
		if filepath.Base(p) == "SKILL.md" {
			alias := filepath.Dir(p) + ".md"
			if hash, err := hashFile(alias); err == nil {
				marker, _ := readMarker(filepath.Join(filepath.Dir(filepath.Dir(p)), bundleMirrorMarkerDir, filepath.Base(alias)+".sha256"))
				if hash == marker {
					e.resourceScope.owned[alias] = hash
				}
			}
		}
	}
}

func (e *Engine) hideParentSkillCollisions(parent *runResourceScope) error {
	if e.bundle == nil || e.bundle.SkillsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(e.bundle.SkillsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == bundleMirrorMarkerDir || (!entry.IsDir() && !strings.HasSuffix(name, ".md")) {
			continue
		}
		stem := strings.TrimSuffix(name, ".md")
		dir := filepath.Join(parent.path, ".claude", "skills", stem)
		for path, hash := range parent.owned {
			if path != dir && path != dir+".md" && !strings.HasPrefix(path, dir+string(filepath.Separator)) {
				continue
			}
			if actual, err := hashFile(path); err == nil && actual == hash {
				if err := os.Remove(path); err != nil {
					return err
				}
			}
		}
		// Remove only empty directories; user-added files still shadow the child.
		removeEmptyResourceDirs(dir)
	}
	return nil
}

func removeEmptyResourceDirs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			removeEmptyResourceDirs(filepath.Join(dir, entry.Name()))
		}
	}
	_ = os.Remove(dir)
}

// A paused child's later resume has no live parent context. Its parent's
// recorded bundle can still prove which unchanged files it supplied; edited
// workspace files do not match and retain workspace-wins precedence.
func (e *Engine) persistedParentResources(ctx context.Context, work string) *runResourceScope {
	owner := &runResourceScope{path: work, owned: map[string]string{}}
	seen := map[string]bool{}
	for id := e.parentRunID; id != "" && !seen[id]; {
		seen[id] = true
		r, err := e.store.LoadRun(ctx, id)
		if err != nil || r == nil {
			break
		}
		rememberPersistedBundleResources(owner, r.BundlePath)
		id = r.ParentRunID
	}
	return owner
}

func rememberPersistedBundleResources(owner *runResourceScope, bundlePath string) {
	if bundlePath == "" {
		return
	}
	var err error
	var b *bundle.Bundle
	if info, statErr := os.Stat(bundlePath); statErr == nil && info.IsDir() {
		b, err = bundle.OpenDir(bundlePath)
	} else {
		var closeBundle func() error
		b, closeBundle, err = bundle.Open(bundlePath, filepath.Join(os.TempDir(), "iterion-parent-resources"))
		if err == nil {
			defer func() { _ = closeBundle() }()
		}
	}
	if err != nil {
		return
	}
	entries, err := os.ReadDir(b.SkillsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		src := filepath.Join(b.SkillsDir, entry.Name())
		dst := filepath.Join(owner.path, ".claude", "skills", entry.Name())
		if entry.IsDir() {
			if same, err := sameTree(src, dst); err == nil && same {
				_ = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
					if err == nil && d.Type().IsRegular() {
						if h, err := hashFile(path); err == nil {
							owner.owned[path] = h
						}
					}
					return nil
				})
			}
		} else if strings.HasSuffix(entry.Name(), ".md") {
			h, err := hashFile(src)
			if err != nil {
				continue
			}
			for _, p := range []string{dst, filepath.Join(strings.TrimSuffix(dst, ".md"), "SKILL.md")} {
				if actual, err := hashFile(p); err == nil && actual == h {
					owner.owned[p] = h
				}
			}
		}
	}
}
