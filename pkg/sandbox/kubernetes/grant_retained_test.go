package kubernetes

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"os"
	"path/filepath"
	"testing"
)

type retainedTestOpener struct {
	root *os.Root
	seen context.Context
}

func (o *retainedTestOpener) OpenRoot(c context.Context) (*os.Root, error) {
	o.seen = c
	if err := c.Err(); err != nil {
		return nil, err
	}
	return o.root.OpenRoot(".")
}
func TestRunFilesGrantDrainRetainsOriginal(t *testing.T) {
	parent := t.TempDir()
	host := filepath.Join(parent, "leaf")
	moved := filepath.Join(parent, "moved")
	if err := os.Mkdir(host, 0755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(host)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	op := &retainedTestOpener{root: root}
	if err = os.Rename(host, moved); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(host, 0755); err != nil {
		t.Fatal(err)
	}
	pod := t.TempDir()
	if err = os.WriteFile(filepath.Join(pod, "report"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	shim, _ := exportShim(t, pod, 0, "")
	r := drainRun([]sandbox.ReadBackDir{{ContainerPath: "/iterion/artifact-files", HostDir: host, RootOpener: op}}, shim)
	if err = r.ExportRunFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(moved, "report"))
	if err != nil || string(got) != "original" {
		t.Fatalf("original: %q %v", got, err)
	}
	if _, err = os.Stat(filepath.Join(host, "report")); !os.IsNotExist(err) {
		t.Fatalf("replacement touched: %v", err)
	}
}
func TestRunFilesGrantDrainCancelledNoFDLeak(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	op := &retainedTestOpener{root: root}
	dir := sandbox.ReadBackDir{HostDir: root.Name(), RootOpener: op}
	before, _ := os.ReadDir("/proc/self/fd")
	for range 20 {
		tree, err := openReadBackDrainTree(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		tree.close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = openReadBackDrainTree(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, _ := os.ReadDir("/proc/self/fd")
	if len(after) != len(before) {
		t.Fatalf("fd delta %d", len(after)-len(before))
	}
}

type nilPrivateRoot struct{}

func (nilPrivateRoot) OpenRoot(context.Context) (*os.Root, error) { return nil, nil }
func TestRunFilesGrantNilRootRefused(t *testing.T) {
	r := drainRun([]sandbox.ReadBackDir{{HostDir: t.TempDir(), ContainerPath: "/iterion/artifact-files", RootOpener: nilPrivateRoot{}}}, Kubectl(""))
	err := r.ExportRunFiles(context.Background())
	if sandbox.ReadBackLossReasonOf(err) != sandbox.ReadBackRefused {
		t.Fatalf("expected typed refusal: %v", err)
	}
}
