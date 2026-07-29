package pluginsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FetchTimeout bounds a single git operation. A plugin fetch sits on the
// launch path, so a hung remote must fail fast and loudly rather than stall a
// run submission indefinitely.
const FetchTimeout = 90 * time.Second

// Fetcher materialises a PluginSource's repository into a local cache
// directory and returns the checkout path.
//
// Caching is keyed by (git_url, ref) and, once resolved, the tree is left in
// place: with a PINNED ref the content is immutable, so the second launch and
// every one after it cost nothing. That is the whole reason the design prefers
// pinning over a moving branch — it collapses "resolve at launch" into a
// no-network operation without introducing a staleness window.
type Fetcher struct {
	// CacheDir roots the checkouts. Ephemeral by design: it is a cache, never
	// the authority — the durable record is the PluginSource in the store, so
	// a cold pod simply re-derives.
	CacheDir string
	// CredentialFor resolves a source's read credential. It returns the
	// secret VALUE, which the fetcher passes to git without logging it.
	// Nil (or a nil return) means "public repository".
	CredentialFor func(ctx context.Context, s PluginSource) (string, error)

	mu       sync.Mutex
	keyGates map[string]chan struct{}
}

// Fetch returns a local path containing the source's repository at its ref.
//
// The checkout is built in a temporary sibling and moved into place with a
// single rename, so `dest` only ever exists fully populated. Building in place
// would publish the directory at `git init` time — a concurrent launch would
// then be handed a tree with a .git and nothing else.
func (f *Fetcher) Fetch(ctx context.Context, s PluginSource) (string, error) {
	if f.CacheDir == "" {
		return "", fmt.Errorf("pluginsource: fetcher has no cache dir")
	}
	key := cacheKey(s)
	dest := filepath.Join(f.CacheDir, key)

	unlock, err := f.lockKey(ctx, key)
	if err != nil {
		return "", fmt.Errorf("pluginsource: waiting on a concurrent fetch of %q: %w", s.Name, err)
	}
	defer unlock()

	// A pinned ref makes the checkout immutable, so an existing tree is
	// authoritative and we skip the network entirely.
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil && s.PinnedRef() {
		return dest, nil
	}
	if err := os.MkdirAll(f.CacheDir, 0o700); err != nil {
		return "", err
	}

	var cred string
	if f.CredentialFor != nil {
		c, err := f.CredentialFor(ctx, s)
		if err != nil {
			return "", fmt.Errorf("pluginsource: resolve credential for %q: %w", s.Name, err)
		}
		cred = c
	}

	staging, err := os.MkdirTemp(f.CacheDir, "."+key+".staging-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging) // no-op once the rename has moved it away

	if err := f.git(ctx, staging, cred, "init", "--quiet"); err != nil {
		return "", err
	}
	if err := f.git(ctx, staging, cred, "remote", "add", "origin", s.GitURL); err != nil {
		return "", err
	}
	if err := f.git(ctx, staging, cred, "fetch", "--depth", "1", "origin", s.Ref); err != nil {
		return "", err
	}
	if err := f.git(ctx, staging, cred, "checkout", "--force", "FETCH_HEAD"); err != nil {
		return "", err
	}
	if err := publish(staging, dest); err != nil {
		return "", fmt.Errorf("pluginsource: publish checkout for %q: %w", s.Name, err)
	}
	return dest, nil
}

// publish moves a finished checkout onto its cache path. A moving ref replaces
// an older tree, so the previous one is renamed aside first and deleted after
// the swap — never before, so a failed rename leaves the old tree serving.
//
// The in-process gate does not cover a second process sharing the cache dir, so
// a publisher that loses the race finds the path already holding an equivalent
// checkout and keeps it rather than failing the launch.
func publish(staging, dest string) error {
	retired := ""
	if _, err := os.Stat(dest); err == nil {
		retired = dest + ".retired-" + filepath.Base(staging)
		if err := os.Rename(dest, retired); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			retired = "" // someone else retired it first
		}
	}
	if err := os.Rename(staging, dest); err != nil {
		if retired != "" {
			_ = os.Rename(retired, dest)
		}
		if _, statErr := os.Stat(filepath.Join(dest, ".git")); statErr == nil {
			return nil
		}
		return err
	}
	if retired != "" {
		_ = os.RemoveAll(retired)
	}
	return nil
}

// lockKey serialises fetches of the same (url, ref) within this process, so N
// concurrent launches on a cold pod cost one clone instead of N racing ones.
//
// Waiting honours ctx. Each attempt is bounded by FetchTimeout, so against a
// hung remote a queue of waiters would otherwise burn that timeout one after
// another, unable to give up even once the launch that wanted them is gone.
func (f *Fetcher) lockKey(ctx context.Context, key string) (func(), error) {
	f.mu.Lock()
	if f.keyGates == nil {
		f.keyGates = map[string]chan struct{}{}
	}
	gate, ok := f.keyGates[key]
	if !ok {
		gate = make(chan struct{}, 1)
		f.keyGates[key] = gate
	}
	f.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// git runs one git command. The credential is injected via an askpass helper
// (never argv, never the URL) so it cannot leak into a process listing, git's
// own logs, or an error message — the same use-by-reference discipline the
// mounted run secrets follow.
func (f *Fetcher) git(ctx context.Context, dir, cred string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never block a launch on an interactive auth prompt
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if cred != "" {
		helper, cleanup, err := askpassHelper(cred)
		if err != nil {
			return err
		}
		defer cleanup()
		cmd.Env = append(cmd.Env, "GIT_ASKPASS="+helper, "GIT_USERNAME=x-access-token")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pluginsource: git %s: %w: %s", strings.Join(args, " "), err, redact(string(out), cred))
	}
	return nil
}

// askpassHelper writes a 0700 script that echoes the credential, so git reads
// it over a pipe instead of receiving it as an argument.
func askpassHelper(cred string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "iterion-ps-askpass-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	p := filepath.Join(dir, "askpass.sh")
	// The credential is embedded in a 0700 file readable only by this
	// process's user, for the lifetime of one git invocation.
	script := "#!/bin/sh\ncase \"$1\" in *[Uu]sername*) printf '%s' \"$GIT_USERNAME\";; *) cat <<'IterionCredEOF'\n" +
		cred + "\nIterionCredEOF\n;; esac\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return p, cleanup, nil
}

// redact strips the credential from command output before it reaches an error
// or a log line.
func redact(s, cred string) string {
	if cred == "" {
		return s
	}
	return strings.ReplaceAll(s, cred, "«redacted»")
}

// cacheKey derives a stable, filesystem-safe directory name for a source's
// checkout. Hashing the URL keeps credentials-in-URL and path separators out
// of the cache layout; the ref is included so two pins of the same repo
// coexist.
func cacheKey(s PluginSource) string {
	h := sha256.Sum256([]byte(s.GitURL + "\x00" + s.Ref))
	return s.Name + "-" + hex.EncodeToString(h[:])[:16]
}
