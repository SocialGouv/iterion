package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The child dies after its Store has read the first byte of a declared file,
// while the content-addressed snapshot is still incomplete and unpublished.
func TestNativeFileCaptureKillChild(t *testing.T) {
	if os.Getenv("ITERION_PORT_FILE_KILL_CHILD") != "1" {
		return
	}
	base := nativeKillChildStore(t)
	s := &blockingPortFileStore{RunStore: base, PortActivationStore: store.AsPortActivationStore(base),
		RunFilesStore: store.AsRunFilesStore(base), marker: os.Getenv("ITERION_PORT_KILL_MARKER")}
	engine := New(portsTestWorkflow(t, portsFileSource), s, portsExecutorFunc(produceOrPassPortFile),
		WithWorkDir(os.Getenv("ITERION_PORT_KILL_WORKDIR")), WithSandboxOverride("none"))
	ctx, cancel := context.WithTimeout(store.WithIdentity(context.Background(), "ports-fixture", "owner"), 30*time.Second)
	defer cancel()
	if err := engine.Run(ctx, "pc1_file_capture_kill", map[string]any{"value": "saved"}); err != nil {
		t.Fatalf("file-capture child returned before SIGKILL: %v", err)
	}
	t.Fatal("file-capture child completed before SIGKILL")
}

type blockingPortFileStore struct {
	store.RunStore
	store.PortActivationStore
	store.RunFilesStore
	marker string
}

func (s *blockingPortFileStore) PortBackendIdentity() string {
	if identified, ok := s.RunStore.(interface{ PortBackendIdentity() string }); ok {
		return identified.PortBackendIdentity()
	}
	return ""
}
func (s *blockingPortFileStore) VerifyPortDistributedActivation(ctx context.Context, record *store.PortActivation, now time.Time) error {
	return verifyWrappedPortsTestActivation(ctx, s.RunStore, record, now)
}
func (s *blockingPortFileStore) LoadPortDistributedProof(ctx context.Context) (*store.PortDistributedProof, error) {
	p, ok := s.RunStore.(store.PortDistributedProofStore)
	if !ok {
		return nil, store.ErrPortActivation
	}
	return p.LoadPortDistributedProof(ctx)
}
func (s *blockingPortFileStore) SavePortDistributedProof(ctx context.Context, revision uint64, proof *store.PortDistributedProof) error {
	p, ok := s.RunStore.(store.PortDistributedProofStore)
	if !ok {
		return store.ErrPortActivation
	}
	return p.SavePortDistributedProof(ctx, revision, proof)
}

func (s *blockingPortFileStore) PutPortFile(ctx context.Context, ref store.PortFileRef, content io.Reader) error {
	return store.AsPortFilesStore(s.RunStore).PutPortFile(ctx, ref, &blockAfterFirstByte{ctx: ctx, source: content, marker: s.marker})
}

type blockAfterFirstByte struct {
	ctx    context.Context
	source io.Reader
	marker string
	read   bool
}

func (r *blockAfterFirstByte) Read(buffer []byte) (int, error) {
	if !r.read {
		if len(buffer) == 0 {
			return 0, nil
		}
		n, err := r.source.Read(buffer[:1])
		r.read = n > 0
		return n, err
	}
	if err := os.WriteFile(r.marker, []byte("one byte read by file store"), 0o600); err != nil {
		return 0, err
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func produceOrPassPortFile(ctx context.Context, _ ir.Node, input map[string]any) (map[string]any, error) {
	if _, producer := input["value"]; producer {
		area, ok := model.InvocationFilesFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("missing producer file area")
		}
		if err := os.WriteFile(filepath.Join(area.HostDir, "report.txt"), []byte("saved\n"), 0o600); err != nil {
			return nil, err
		}
		return map[string]any{"report": "report.txt"}, nil
	}
	return map[string]any{"report": input["report"]}, nil
}

func TestNativeFileCaptureKillRecoveryFilesystem(t *testing.T) {
	testNativeFileCaptureKillRecovery(t, tmpStore, nil)
}

func TestNativeFileCaptureKillRecoveryMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("required Mongo file-capture kill test needs ITERION_TEST_MONGO_URI")
		}
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	factory, childEnv := nativeProcessKillMongoFixture(t, uri)
	testNativeFileCaptureKillRecovery(t, factory, childEnv)
}

func testNativeFileCaptureKillRecovery(t *testing.T, factory portsTestStoreFactory, extraChildEnv func() []string) {
	engine, s := portsTestEngine(t, factory, portsFileSource, portsExecutorFunc(produceOrPassPortFile))
	marker := filepath.Join(t.TempDir(), "capture-started")
	child := exec.Command(os.Args[0], "-test.run=^TestNativeFileCaptureKillChild$")
	child.Env = append(os.Environ(),
		"ITERION_PORT_FILE_KILL_CHILD=1",
		"ITERION_PORT_KILL_STORE="+s.Root(),
		"ITERION_PORT_KILL_MARKER="+marker,
		"ITERION_PORT_KILL_WORKDIR="+engine.workDir,
	)
	if extraChildEnv != nil {
		child.Env = append(child.Env, extraChildEnv()...)
	}
	var childOutput bytes.Buffer
	child.Stdout, child.Stderr = &childOutput, &childOutput
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
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child never began file capture: %s", childOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("file-capture child was not killed")
	}
	stopped = true
	ctx, cancel := context.WithTimeout(store.WithIdentity(context.Background(), "ports-fixture", "owner"), 45*time.Second)
	defer cancel()
	r := portsTestRun(t, s, "pc1_file_capture_kill")
	if r.Status != store.RunStatusRunning || r.PortExecution.Invocations["produce"].Status != store.PortRunning {
		t.Fatalf("process did not die during the running producer: %+v", r)
	}
	visible, err := store.PublishedPortFileRefs(ctx, s, r.ID)
	if err != nil || len(visible) != 0 {
		t.Fatalf("partially captured bytes became public: refs=%+v err=%v", visible, err)
	}
	changed, err := s.UpdateRunStatusIf(ctx, r.ID, store.RunStatusFailedResumable, "worker killed during file capture", []store.RunStatus{store.RunStatusRunning})
	if err != nil || !changed {
		t.Fatalf("mark dead process resumable: %v changed=%v", err, changed)
	}
	resume := New(portsTestWorkflow(t, portsFileSource), s, portsExecutorFunc(produceOrPassPortFile),
		WithWorkDir(engine.workDir), WithSandboxOverride("none"))
	if err := resume.Resume(ctx, r.ID, nil); err != nil {
		t.Fatal(err)
	}
	r = portsTestRun(t, s, r.ID)
	if r.Status != store.RunStatusFinished || r.PortExecution.Invocations["produce"].Attempt != 2 || r.PortExecution.Invocations["pass"].Status != store.PortSucceeded {
		t.Fatalf("file producer did not recover and publish: %+v", r)
	}
	visible, err = store.PublishedPortFileRefs(ctx, s, r.ID)
	if err != nil || len(visible) != 1 {
		t.Fatalf("expected one committed file: refs=%+v err=%v", visible, err)
	}
	for _, ref := range visible {
		body, _, err := store.AsRunFilesStore(s).OpenRunFile(ctx, r.ID, ref.Path)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(body)
		if err := errors.Join(readErr, body.Close()); err != nil {
			t.Fatal(err)
		}
		if string(data) != "saved\n" || !strings.HasPrefix(ref.Path, "published/produce/2/") {
			t.Fatalf("recovered file has stale bytes or attempt: %+v %q", ref, data)
		}
	}
}
