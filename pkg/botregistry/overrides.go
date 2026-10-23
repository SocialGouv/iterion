package botregistry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/store"
)

// overridesRelPath is the workspace-local file that overrides per-bot
// catalog visibility without editing a bot's manifest. It lives under
// the already-gitignored .iterion/ store dir, so toggling a shipped
// (git-tracked) bot off in a workspace produces no source churn. The
// studio Catalog manager writes it; operators may hand-edit it. Overlay
// state WINS over the manifest's `enabled` default.
const overridesRelPath = ".iterion/bot-overrides.yaml"

// Overrides is the parsed workspace overlay. Keys are canonicalised bot
// names (NormalizeName); each entry may pin `enabled` either way, so the
// overlay can both hide a manifest-enabled bot and re-expose a
// manifest-disabled one.
type Overrides struct {
	Bots map[string]BotOverride `yaml:"bots,omitempty"`
}

// BotOverride is the per-bot workspace override. A nil Enabled means the
// entry carries no visibility decision (the manifest default stands).
type BotOverride struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// overridesMaxBytes is the most of the overlay file that is read: the size
// cap a `.bot` source and an author document are held to.
const overridesMaxBytes = 10 << 20

// OverlayRefusal is the workspace overlay refused before it is decoded: past
// the size cap, or holding more values than the read budget lets a decode
// build (workflowfile.CheckYAMLDecode). The overlay is the workspace's —
// the studio's Catalog manager writes it (SetOverlayEnabled), an operator
// may hand-edit it, and any process that writes the workspace's .iterion/
// can — and every catalogue listing reads it, so a decode no bound held
// would spend its memory on every /api/bots request.
type OverlayRefusal struct {
	Path string
	// Reason says what bound it passed, positioned when the decode guard
	// placed it.
	Reason string
}

// Error names the overlay by its workspace-relative name (Path holds where
// it is): the refusal travels to every door that refuses on it, HTTP answers
// included, and the workspace's root is no business of theirs.
func (r *OverlayRefusal) Error() string {
	return fmt.Sprintf("botregistry: the workspace overlay %s is not read: %s", overridesRelPath, r.Reason)
}

// LoadOverrides reads <workdir>/.iterion/bot-overrides.yaml. A missing
// file is not an error — it returns an empty, non-nil Overrides. One past
// the size cap or the decode's bound is an *OverlayRefusal, before yaml.v2
// builds a node of it; one that is not YAML is the decode's error.
//
// Every catalogue listing reads the overlay, and at the decode's bound one
// decoding builds hundreds of MiB, so the answer is kept for the bytes it
// was read from (overlayCache): a listing that finds the same bytes, under
// the same read budget, takes it without decoding them again, and listings
// that find new bytes at once wait for one decoding of them. The Overrides
// answered is shared by those listings: read it, never modify it
// (SetOverlayEnabled decodes its own).
func LoadOverrides(workdir string) (*Overrides, error) {
	path := filepath.Join(workdir, overridesRelPath)
	f, err := os.Open(path) // #nosec G304 -- the workspace's own overlay
	if err != nil {
		if os.IsNotExist(err) {
			return &Overrides{}, nil
		}
		return nil, fmt.Errorf("botregistry: read overrides %s: %w", path, err)
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(f, overridesMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("botregistry: read overrides %s: %w", path, err)
	}
	if n > overridesMaxBytes {
		return nil, oversizeOverlay(path)
	}
	key := overlayKey{budget: parser.ReadBudget()}
	copy(key.digest[:], sum.Sum(nil))
	overlayCacheMu.Lock()
	defer overlayCacheMu.Unlock()
	if c, ok := overlayCache[path]; ok && c.key == key {
		return c.ov, c.err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("botregistry: read overrides %s: %w", path, err)
	}
	body, err := io.ReadAll(io.LimitReader(f, overridesMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("botregistry: read overrides %s: %w", path, err)
	}
	ov, err := decodeOverrides(path, body)
	// Kept under the bytes decoded, which the file may have changed to
	// since they were summed above.
	key.digest = sha256.Sum256(body)
	if len(overlayCache) >= overlayCacheSize {
		clear(overlayCache)
	}
	overlayCache[path] = overlayCached{key: key, ov: ov, err: err}
	return ov, err
}

// readOverrides is LoadOverrides without the cache: a decoding of its own,
// which its caller may modify (SetOverlayEnabled).
func readOverrides(workdir string) (*Overrides, error) {
	path := filepath.Join(workdir, overridesRelPath)
	f, err := os.Open(path) // #nosec G304 -- the workspace's own overlay
	if err != nil {
		if os.IsNotExist(err) {
			return &Overrides{}, nil
		}
		return nil, fmt.Errorf("botregistry: read overrides %s: %w", path, err)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, overridesMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("botregistry: read overrides %s: %w", path, err)
	}
	return decodeOverrides(path, body)
}

// decodeOverrides decodes the overlay's bytes, held to its size cap and to
// the decode's bound before yaml.v2 builds a node of them.
func decodeOverrides(path string, body []byte) (*Overrides, error) {
	if len(body) > overridesMaxBytes {
		return nil, oversizeOverlay(path)
	}
	if r := workflowfile.CheckYAMLDecode(body, parser.ReadBudget(), false); r != nil {
		return nil, &OverlayRefusal{Path: path, Reason: r.Error() + " — " + parser.ReadBudgetHint}
	}
	var ov Overrides
	if err := yaml.Unmarshal(body, &ov); err != nil {
		return nil, fmt.Errorf("botregistry: parse overrides %s: %w", path, err)
	}
	return &ov, nil
}

func oversizeOverlay(path string) *OverlayRefusal {
	return &OverlayRefusal{Path: path, Reason: fmt.Sprintf("it exceeds the maximum size of %d bytes", overridesMaxBytes)}
}

// overlayCache is LoadOverrides' answer for each overlay path, kept with the
// key it was read under; overlayCacheMu serialises its use and every
// decoding it keeps. It holds the overlays of at most overlayCacheSize
// workspaces — a server switches between a few — and starts over past it.
var (
	overlayCacheMu sync.Mutex
	overlayCache   = map[string]overlayCached{}
)

const overlayCacheSize = 16

// overlayKey is what an overlay's answer depends on: its bytes (their
// sha256) and the read budget its decode was held to.
type overlayKey struct {
	digest [32]byte
	budget int
}

type overlayCached struct {
	key overlayKey
	ov  *Overrides
	err error
}

// lookup returns the override for name (matched by normalized name).
func (o *Overrides) lookup(name string) (BotOverride, bool) {
	if o == nil || o.Bots == nil {
		return BotOverride{}, false
	}
	bo, ok := o.Bots[NormalizeName(name)]
	return bo, ok
}

// ResolveEnabled composes a bot's manifest `enabled` default with the
// workspace overlay: an overlay entry that pins `enabled` WINS, otherwise
// the manifest default stands. ov may be nil (= no overlay).
func ResolveEnabled(name string, manifestEnabled bool, ov *Overrides) bool {
	if bo, ok := ov.lookup(name); ok && bo.Enabled != nil {
		return *bo.Enabled
	}
	return manifestEnabled
}

// overlayLockWait is how long a toggle waits for the overlay's lock file
// (overlayLockPath) when the caller's context sets no earlier end: the lock
// is held across one reading and one write of a small file, so a holder
// that keeps it longer is stuck, not busy.
var overlayLockWait = 30 * time.Second

// overlayWriteHook is a test seam: called by SetOverlayEnabled between its
// reading of the overlay and its write. Nil outside tests.
var overlayWriteHook func()

// overlayLockPath is the lock file every overlay writer holds across its
// read-modify-write, under the gitignored .iterion/ beside the overlay.
func overlayLockPath(workdir string) string {
	return filepath.Join(workdir, overridesRelPath+".lock")
}

// SetOverlayEnabled records a per-bot visibility override and persists it
// atomically. enabled==nil clears any override (the manifest default
// stands again); a non-nil enabled pins the bot on/off in this workspace.
//
// The file is rewritten whole from a reading of it, so the reading and the
// write are one critical section, held by the lock file every overlay
// writer takes (overlayLockPath) — this process's other writers included,
// each holding the lock through a handle of its own, which the lock
// serialises like another process's (flock(2); gofrs/flock queues the
// same-inode lockers of a process where it is fcntl; LockFileEx on
// Windows): two toggles at once are applied one after the other, each over
// the file the other left — a toggle read before another's write would
// rewrite the file without it. The wait for the lock ends with ctx, or
// after overlayLockWait: a holder that never lets go fails the toggle, it
// does not hang it. An overlay that is not read (*OverlayRefusal) is not
// written either: rewritten from what was not read, every override it holds
// would go.
func SetOverlayEnabled(ctx context.Context, workdir, name string, enabled *bool) error {
	dir := filepath.Join(workdir, ".iterion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("botregistry: mkdir %s: %w", dir, err)
	}
	lock := flock.New(overlayLockPath(workdir))
	wait, cancel := context.WithTimeout(ctx, overlayLockWait)
	defer cancel()
	locked, err := lock.TryLockContext(wait, 20*time.Millisecond)
	if err == nil && !locked {
		err = wait.Err()
	}
	if err != nil {
		return fmt.Errorf("botregistry: the workspace overlay's lock %s is held by another writer: %w", overridesRelPath+".lock", err)
	}
	defer func() { _ = lock.Unlock() }()
	ov, err := readOverrides(workdir)
	if err != nil {
		return err
	}
	if overlayWriteHook != nil {
		overlayWriteHook()
	}
	if ov.Bots == nil {
		ov.Bots = map[string]BotOverride{}
	}
	key := NormalizeName(name)
	if enabled == nil {
		delete(ov.Bots, key)
	} else {
		ov.Bots[key] = BotOverride{Enabled: enabled}
	}
	return writeOverrides(workdir, ov)
}

// writeOverrides marshals ov to <workdir>/.iterion/bot-overrides.yaml
// durably (atomic temp + fsync + rename via the shared store writer).
func writeOverrides(workdir string, ov *Overrides) error {
	dir := filepath.Join(workdir, ".iterion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("botregistry: mkdir %s: %w", dir, err)
	}
	body, err := yaml.Marshal(ov)
	if err != nil {
		return fmt.Errorf("botregistry: marshal overrides: %w", err)
	}
	path := filepath.Join(workdir, overridesRelPath)
	if err := store.WriteFileAtomic(path, body, 0o644); err != nil {
		return fmt.Errorf("botregistry: write overrides %s: %w", path, err)
	}
	return nil
}
