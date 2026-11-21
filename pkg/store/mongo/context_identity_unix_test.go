//go:build unix

package mongo

import (
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo/address"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/internal/testfs"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestMongoContextIdentityKeepsUnixSymlinkDotDotDestinations(t *testing.T) {
	// The kernel limits Unix socket path length; a full t.TempDir test name
	// can exhaust it before the fixture's own path is appended.
	root, err := os.MkdirTemp("/tmp", "mongo-id-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testfs.RemoveAll(t, root, root) })
	t.Chdir(root)
	for _, dir := range []string{"a", "b/inner"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "b/inner"), filepath.Join(root, "a/link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		listener, err := net.Listen("unix", filepath.Join(root, name, "db.sock"))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_, _ = io.WriteString(conn, name)
				_ = conn.Close()
			}
		}()
		t.Cleanup(func() { _ = listener.Close(); <-done })
	}
	ctx := store.WithTenant(t.Context(), "team")
	for _, relative := range []bool{false, true} {
		paths := []string{root + "/a/link/../db.sock", root + "/a/db.sock"}
		if relative {
			paths = []string{"a/link/../db.sock", "a/db.sock"}
		}
		var refs []store.ContextRef
		for i, path := range paths {
			opts := options.Client().ApplyURI("mongodb://" + url.PathEscape(path) + "/")
			if err := opts.Validate(); err != nil {
				t.Fatal(err)
			}
			native := address.Address(opts.Hosts[0]).Canonicalize()
			conn, err := (&net.Dialer{}).DialContext(ctx, native.Network(), native.String())
			if err != nil {
				t.Fatal(err)
			}
			if deadline, ok := t.Deadline(); ok {
				if err := conn.SetDeadline(deadline); err != nil {
					_ = conn.Close()
					t.Fatal(err)
				}
			}
			got, err := io.ReadAll(conn)
			_ = conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"b", "a"}[i]; string(got) != want {
				t.Fatalf("native endpoint=%q want %q", got, want)
			}
			ns, err := configuredContextNamespace(opts, "runs")
			if err != nil {
				t.Fatal(err)
			}
			ref, err := (&Store{contextNamespace: ns}).ContextIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			refs = append(refs, ref)
		}
		if refs[0].ID == refs[1].ID {
			t.Errorf("distinct native Unix endpoints collapse after lexical path cleaning (relative=%v)", relative)
		}
	}
}
