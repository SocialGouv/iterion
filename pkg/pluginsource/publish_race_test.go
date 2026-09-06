package pluginsource

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A peer that publishes the same PINNED checkout while this fetch is staging has
// published exactly the tree this fetch wanted: the cache path is
// content-addressed and the ref immutable. The loser must take it — and must
// NOT swap its own copy in, because renaming the peer's tree aside leaves
// `dest` ABSENT for an instant, and that instant is the window a THIRD
// publisher's "was it already published?" read falls into: its own rename has
// already lost, so it finds nothing and reports ENOTEMPTY for a tree that is
// there (#854, which ejected a PR from the merge queue).
//
// The interleaving is scripted through the fetcher's beforePublish seam, not
// wagered on `-count`: the peer publishes at the one instant that matters.
func TestFetcher_PeerPublishedTheSamePinnedCheckoutFirst(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	origin := gitOrigin(t, "v1.0.0")
	cache := t.TempDir()
	s := validSource()
	s.GitURL, s.Ref = origin, "v1.0.0"

	const peerMark = ".published-by-the-peer"
	peer := &Fetcher{CacheDir: cache}
	f := &Fetcher{CacheDir: cache}
	published := false
	f.beforePublish = func(_, dest string) {
		if published {
			return
		}
		published = true
		if _, err := peer.Fetch(context.Background(), s); err != nil {
			t.Errorf("the peer's own fetch failed, so this test proves nothing: %v", err)
			return
		}
		// Marks the peer's inode, so "was it kept?" is answerable after the
		// fact — both trees have identical content otherwise.
		if err := os.WriteFile(filepath.Join(dest, peerMark), nil, 0o644); err != nil {
			t.Errorf("mark the peer's tree: %v", err)
		}
	}

	got, err := f.Fetch(context.Background(), s)
	if err != nil {
		t.Fatalf("a peer publishing the same pinned checkout first must not fail this fetch: %v", err)
	}
	if body, rerr := os.ReadFile(filepath.Join(got, "skills", "deploy-target.md")); rerr != nil || string(body) != "playbook\n" {
		t.Fatalf("the returned path is not a complete checkout: %v %q", rerr, body)
	}
	if _, serr := os.Stat(filepath.Join(got, peerMark)); serr != nil {
		t.Fatalf("the peer's published tree was replaced: renaming an immutable tree aside makes %q ABSENT to every other publisher, which is the window #854 fell into", got)
	}

	// Nothing parked beside it either: a retired or staging copy of a full
	// checkout that nobody deletes is the cache growing one clone per race.
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left behind in the cache dir: %s", e.Name())
		}
	}
}

// The loser's own read of the final path is the second half of the rule: a
// rename that lost to a peer is success when the path holds a complete
// checkout, and an error — carrying the rename's own cause — when it does not.
func TestPublish_LostRenameReadsTheFinalPath(t *testing.T) {
	t.Run("complete final is kept, not swapped", func(t *testing.T) {
		root := t.TempDir()
		staging, dest := filepath.Join(root, "staging"), filepath.Join(root, "dest")
		mkCheckout(t, staging, "ours")
		mkCheckout(t, dest, "the-peers")
		// replaceExisting=false is the immutable case: the peer's tree stands,
		// untouched — asserted by WHOSE tree is there, since "still a checkout"
		// would also hold after a swap.
		if err := (&Fetcher{}).publish(staging, dest, false); err != nil {
			t.Fatalf("a complete final path is the tree this publish wanted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dest, "the-peers")); err != nil {
			t.Fatalf("the peer's tree was swapped out instead of accepted: %v", err)
		}
	})

	t.Run("incomplete final keeps the rename's cause", func(t *testing.T) {
		root := t.TempDir()
		staging, dest := filepath.Join(root, "staging"), filepath.Join(root, "dest")
		mkCheckout(t, staging, "ours")
		// A directory that exists and holds something, but is NOT a checkout:
		// the retire moves it aside, the rename then wins, and the tree that
		// gets published is ours — never a silent accept of a partial tree.
		if err := os.MkdirAll(filepath.Join(dest, "half-written"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := (&Fetcher{}).publish(staging, dest, false); err != nil {
			t.Fatalf("publish over an incomplete final: %v", err)
		}
		if !isPublished(dest) {
			t.Fatal("an incomplete final path must be replaced by the staged checkout, not kept")
		}
	})
}

// Retiring a tree makes `dest` ABSENT for an instant, and that absence is what
// #854's loser read. So a publish of an IMMUTABLE ref must never rename a
// complete tree aside — not even one that appeared after it looked. Deciding on
// a read cannot promise that (the read is stale one syscall later); only making
// the rename itself the test-and-set can.
//
// Both subtests drive the interleaving through the beforeRetire seam, so the
// window is scripted rather than wagered on a `-count` stress run.
func TestPublish_NeverRetiresACompleteImmutableTree(t *testing.T) {
	t.Run("an absent final is taken by the rename, not by a read then a retire", func(t *testing.T) {
		root := t.TempDir()
		staging, dest := filepath.Join(root, "staging"), filepath.Join(root, "dest")
		mkCheckout(t, staging, "ours")

		// dest is absent — the one state a read-then-retire design re-decides
		// on: it reads "nothing there", a peer publishes, and the retire that
		// follows moves that complete tree aside. Reaching the retire AT ALL
		// from here is the defect, so that is what is asserted; a rename that
		// lands cannot have raced anyone.
		f := &Fetcher{}
		retired := false
		f.beforeRetire = func(string) { retired = true }
		if err := f.publish(staging, dest, false); err != nil {
			t.Fatalf("publish onto an absent final: %v", err)
		}
		if retired {
			t.Fatal("publish took a read and then retired: a peer publishing in that gap has its immutable tree renamed aside, and the next publisher's read finds the path ABSENT (#854)")
		}
		if _, err := os.Stat(filepath.Join(dest, "ours")); err != nil {
			t.Fatalf("the staged tree did not land: %v", err)
		}
	})

	t.Run("a peer that repairs an incomplete final is accepted, not retired", func(t *testing.T) {
		root := t.TempDir()
		staging, dest := filepath.Join(root, "staging"), filepath.Join(root, "dest")
		mkCheckout(t, staging, "ours")
		// The one state that still reaches the retire: dest exists and is NOT a
		// checkout. A peer repairs it in that gap — what it published is
		// byte-for-byte what we would have, so it must be kept.
		if err := os.MkdirAll(filepath.Join(dest, "half-written"), 0o755); err != nil {
			t.Fatal(err)
		}
		f := &Fetcher{}
		f.beforeRetire = func(dest string) {
			if err := os.RemoveAll(dest); err != nil {
				t.Errorf("stand in for the peer's publish: %v", err)
				return
			}
			mkCheckout(t, dest, "the-peers")
		}
		if err := f.publish(staging, dest, false); err != nil {
			t.Fatalf("publish onto a final a peer repaired: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dest, "the-peers")); err != nil {
			t.Fatalf("a complete immutable tree was renamed aside anyway: %v", err)
		}
		assertNoLeftovers(t, root, "staging", "dest")
	})
}

// assertNoLeftovers fails when dir holds anything but the named entries: a
// retired or staging copy of a full checkout that nobody deletes is the cache
// growing one clone per race. Names are matched WHOLE — a retired copy is
// `<key>.retired-…`, which no prefix filter over "." can catch.
func assertNoLeftovers(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, w := range want {
		keep[w] = true
	}
	var extra []string
	for _, e := range entries {
		if !keep[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	if len(extra) > 0 {
		t.Errorf("left behind in %s: %s", dir, strings.Join(extra, ", "))
	}
}

// mkCheckout builds a directory isPublished accepts, marked so a later
// assertion can tell WHOSE tree ended up at a path.
func mkCheckout(t *testing.T, dir string, marks ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range marks {
		if err := os.WriteFile(filepath.Join(dir, m), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
