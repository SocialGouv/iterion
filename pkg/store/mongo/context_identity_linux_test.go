//go:build linux

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

func TestMongoContextIdentityAbstractSocketIgnoresWorkingDirectory(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "r3-abstract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testfs.RemoveAll(t, root, root) })
	name := "@iterion-" + filepath.Base(root) + ".sock"
	listener, err := net.Listen("unix", name)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "same abstract endpoint")
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	ctx := store.WithTenant(t.Context(), "team")
	refs := map[string][]store.ContextRef{}
	for _, dir := range []string{"a", "b"} {
		cwd := filepath.Join(root, dir)
		if err := os.Mkdir(cwd, 0700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(cwd)
		for _, spelling := range []string{name, "\x00" + name[1:]} {
			opts := options.Client().ApplyURI("mongodb://" + url.QueryEscape(spelling) + "/")
			if err := opts.Validate(); err != nil {
				t.Fatal(err)
			}
			native := address.Address(opts.Hosts[0]).Canonicalize()
			c, err := (&net.Dialer{}).DialContext(ctx, native.Network(), native.String())
			if err != nil {
				t.Fatal(err)
			}
			if deadline, ok := t.Deadline(); ok {
				if err := c.SetDeadline(deadline); err != nil {
					_ = c.Close()
					t.Fatal(err)
				}
			}
			data, err := io.ReadAll(c)
			_ = c.Close()
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "same abstract endpoint" {
				t.Fatalf("endpoint=%q", data)
			}
			ns, err := configuredContextNamespace(opts, "runs")
			if err != nil {
				t.Fatal(err)
			}
			ref, err := (&Store{contextNamespace: ns}).ContextIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("cwd=%s native=%q endpoint=%q id=%s hosts=%v", cwd, native.String(), data, ref.ID, ns.Hosts)
			refs[spelling] = append(refs[spelling], ref)
		}
	}
	for spelling, rr := range refs {
		if rr[0].ID != rr[1].ID {
			t.Errorf("same configured abstract socket %q acquires a different identity across CWDs", spelling)
		}
	}
}

func TestMongoContextIdentityAtNamedFilesystemSocketKeepsWorkingDirectory(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "r3-file-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testfs.RemoveAll(t, root, root) })
	ctx := store.WithTenant(t.Context(), "team")
	var refs []store.ContextRef
	for _, dir := range []string{"a", "b"} {
		cwd := filepath.Join(root, dir)
		if err := os.Mkdir(cwd, 0700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", filepath.Join(cwd, "@same.sock"))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				c, err := listener.Accept()
				if err != nil {
					return
				}
				_, _ = io.WriteString(c, dir)
				_ = c.Close()
			}
		}()
		t.Cleanup(func() { _ = listener.Close(); <-done })
		t.Chdir(cwd)
		opts := options.Client().ApplyURI("mongodb://" + url.QueryEscape("./@same.sock") + "/")
		if err := opts.Validate(); err != nil {
			t.Fatal(err)
		}
		native := address.Address(opts.Hosts[0]).Canonicalize()
		c, err := (&net.Dialer{}).DialContext(ctx, native.Network(), native.String())
		if err != nil {
			t.Fatal(err)
		}
		if deadline, ok := t.Deadline(); ok {
			if err := c.SetDeadline(deadline); err != nil {
				_ = c.Close()
				t.Fatal(err)
			}
		}
		data, err := io.ReadAll(c)
		_ = c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != dir {
			t.Fatalf("endpoint=%q want=%s", data, dir)
		}
		ns, err := configuredContextNamespace(opts, "runs")
		if err != nil {
			t.Fatal(err)
		}
		ref, err := (&Store{contextNamespace: ns}).ContextIdentity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("native=%q endpoint=%q id=%s hosts=%v", native.String(), data, ref.ID, ns.Hosts)
		refs = append(refs, ref)
	}
	if refs[0].ID == refs[1].ID {
		t.Errorf("distinct filesystem sockets acquire the same identity")
	}
}
