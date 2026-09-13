package runview

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestNativeOrphanProcessChild(t *testing.T) {
	if os.Getenv("ITERION_NATIVE_ORPHAN_CHILD") != "1" {
		return
	}
	s, err := store.OpenExisting(os.Getenv("ITERION_NATIVE_ORPHAN_STORE"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := s.LockRun(context.Background(), "pc1_orphan_process")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if err := os.WriteFile(os.Getenv("ITERION_NATIVE_ORPHAN_MARKER"), []byte("locked"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second) // parent kills this process while it owns the run lock
}

func TestNativeOrphanProcessBecomesResumableAfterKill(t *testing.T) {
	dir := t.TempDir()
	seed, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const id = "pc1_orphan_process"
	ctx := store.WithRuntimeSemantics(context.Background(), store.RuntimeSemanticsPortsV1)
	if _, err := seed.CreateRun(ctx, id, "fixture", nil); err != nil {
		t.Fatal(err)
	}
	identity := store.PortExecutionIdentity{Source: "fixture", Graph: "fixture", Contract: "fixture", Policy: "fixture", Inputs: "fixture"}
	if err := store.SavePortExecution(ctx, seed, id, 0, &store.PortExecution{
		Version: store.PortExecutionVersion, Revision: 1, Generation: 1, RootRunID: id, Identity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	backdateRun(t, seed, id)
	marker := filepath.Join(t.TempDir(), "lock-held")
	child := exec.Command(os.Args[0], "-test.run=^TestNativeOrphanProcessChild$")
	child.Env = append(os.Environ(), "ITERION_NATIVE_ORPHAN_CHILD=1", "ITERION_NATIVE_ORPHAN_STORE="+dir, "ITERION_NATIVE_ORPHAN_MARKER="+marker)
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not lock the native run: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	service, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.StopBackground(context.Background()) })
	live, err := seed.LoadRun(context.Background(), id)
	if err != nil || live.Status != store.RunStatusRunning {
		t.Fatalf("live process was mistaken for an orphan: run=%+v err=%v", live, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("child was not killed")
	}
	stopped = true
	service.reconcileOrphans(context.Background())
	orphan, err := seed.LoadRun(context.Background(), id)
	if err != nil || orphan.Status != store.RunStatusFailedResumable || orphan.PortExecution == nil {
		t.Fatalf("dead native process was not made resumable: run=%+v err=%v", orphan, err)
	}
}
