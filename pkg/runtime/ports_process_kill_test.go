package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/internal/s3test"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	storemongo "github.com/SocialGouv/iterion/pkg/store/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestNativeProcessKillChild runs only in the deliberately killed subprocess.
// The marker is written from inside the effect body, after the native running
// checkpoint and effect-dispatched marker were committed.
func TestNativeProcessKillChild(t *testing.T) {
	if os.Getenv("ITERION_PORT_KILL_CHILD") != "1" {
		return
	}
	var s store.RunStore
	if uri := os.Getenv("ITERION_PORT_KILL_MONGO_URI"); uri != "" {
		ctx := context.Background()
		objects, err := blob.NewS3(ctx, blob.Config{Bucket: "native-kill", Region: "us-east-1",
			Endpoint: os.Getenv("ITERION_PORT_KILL_S3_ENDPOINT"), UsePathStyle: true,
			AccessKeyID: "fixture", SecretAccessKey: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		mongoStore, err := storemongo.New(ctx, storemongo.Config{URI: uri,
			Database: os.Getenv("ITERION_PORT_KILL_MONGO_DB"), Blob: objects,
			RunFilesScratchDir: os.Getenv("ITERION_PORT_KILL_SCRATCH")})
		if err != nil {
			t.Fatal(err)
		}
		defer mongoStore.Close(ctx)
		s = mongoStore
	} else {
		local, err := store.OpenExisting(os.Getenv("ITERION_PORT_KILL_STORE"))
		if err != nil {
			t.Fatal(err)
		}
		s = local
	}
	executor := portsExecutorFunc(func(ctx context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
		if err := os.WriteFile(os.Getenv("ITERION_PORT_KILL_MARKER"), []byte("effect started"), 0o600); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	engine := New(portsTestWorkflow(t, portsEffectSource("manual")), s, executor,
		WithWorkDir(os.Getenv("ITERION_PORT_KILL_WORKDIR")), WithSandboxOverride("none"))
	ctx, cancel := context.WithTimeout(store.WithIdentity(context.Background(), "ports-fixture", "owner"), 30*time.Second)
	defer cancel()
	if err := engine.Run(ctx, "pc1_process_kill", map[string]any{"items": []any{"a"}}); err != nil {
		t.Fatalf("child returned before SIGKILL: %v", err)
	}
	t.Fatal("child completed before SIGKILL")
}

func TestNativeProcessKillRecoveryFilesystem(t *testing.T) {
	testNativeProcessKillRecovery(t, tmpStore, nil)
}

func TestNativeProcessKillRecoveryMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("required process-kill Mongo test needs ITERION_TEST_MONGO_URI")
		}
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	_, gateway := s3test.New(t, "native-kill")
	database := "iterion_native_kill_" + bson.NewObjectID().Hex()
	scratch := filepath.Join(t.TempDir(), "runfiles")
	factory := func(t *testing.T) store.RunStore {
		ctx, cancel := mongotest.Ctx(t)
		defer cancel()
		objects, err := blob.NewS3(ctx, blob.Config{Bucket: "native-kill", Region: "us-east-1",
			Endpoint: gateway.URL, UsePathStyle: true, AccessKeyID: "fixture", SecretAccessKey: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		s, err := storemongo.New(ctx, storemongo.Config{URI: uri, Database: database, Blob: objects, RunFilesScratchDir: scratch})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := mongotest.TeardownCtx()
			defer cancel()
			if err := s.DB().Drop(ctx); err != nil {
				t.Error(err)
			}
			if err := s.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		return s
	}
	testNativeProcessKillRecovery(t, factory, func() []string {
		return []string{
			"ITERION_PORT_KILL_MONGO_URI=" + uri,
			"ITERION_PORT_KILL_MONGO_DB=" + database,
			"ITERION_PORT_KILL_S3_ENDPOINT=" + gateway.URL,
			"ITERION_PORT_KILL_SCRATCH=" + scratch,
		}
	})
}

func testNativeProcessKillRecovery(t *testing.T, factory portsTestStoreFactory, extraChildEnv func() []string) {
	source := portsEffectSource("manual")
	engine, s := portsTestEngine(t, factory, source, newStubExecutor())
	marker := filepath.Join(t.TempDir(), "effect-started")
	child := exec.Command(os.Args[0], "-test.run=^TestNativeProcessKillChild$")
	child.Env = append(os.Environ(),
		"ITERION_PORT_KILL_CHILD=1",
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
			_ = child.Process.Kill()
			_ = child.Wait()
			stopped = true
			t.Fatalf("child never entered the effect: %s", childOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("child was not killed")
	}
	stopped = true
	ctx, cancel := context.WithTimeout(store.WithIdentity(context.Background(), "ports-fixture", "owner"), 45*time.Second)
	defer cancel()
	r := portsTestRun(t, s, "pc1_process_kill")
	if r.Status != store.RunStatusRunning || r.PortExecution.Invocations["@render[0]"].Status != store.PortRunning ||
		!r.PortExecution.Invocations["@render[0]"].EffectDispatched {
		t.Fatalf("process died outside a committed effect boundary: %+v", r)
	}
	// A supervisor has observed the dead process and makes the orphaned run
	// resumable. The test exercises the real post-kill checkpoint; it does not
	// claim that this helper detects a dead process in production.
	changed, err := s.UpdateRunStatusIf(ctx, r.ID, store.RunStatusFailedResumable, "worker process killed", []store.RunStatus{store.RunStatusRunning})
	if err != nil || !changed {
		t.Fatalf("mark dead process resumable: %v changed=%v", err, changed)
	}
	var replayCalls atomic.Int32
	replay := portsExecutorFunc(func(_ context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
		replayCalls.Add(1)
		return map[string]any{"text": "recovered"}, nil
	})
	resume := New(portsTestWorkflow(t, source), s, replay,
		WithWorkDir(engine.workDir), WithSandboxOverride("none"))
	if err := resume.Resume(ctx, r.ID, nil); !errors.Is(err, ErrPortEffectUncertain) {
		t.Fatalf("killed effect was replayed without evidence: %v", err)
	}
	if replayCalls.Load() != 0 {
		t.Fatal("uncertain effect executed before an operator decision")
	}
	r = portsTestRun(t, s, r.ID)
	next, err := r.PortExecution.Clone()
	if err != nil {
		t.Fatal(err)
	}
	next.Revision++
	next.Invocations["@render[0]"].RecoveryDecision = "fixture operator checked the external ledger: no request was applied"
	next.Invocations["@render[0]"].RecoveryAttempt = 1
	if err := store.SavePortExecution(ctx, s, r.ID, r.PortExecution.Revision, next); err != nil {
		t.Fatal(err)
	}
	if err := resume.Resume(ctx, r.ID, nil); err != nil {
		t.Fatal(err)
	}
	r = portsTestRun(t, s, r.ID)
	if replayCalls.Load() != 1 || r.PortExecution.Invocations["@render[0]"].Attempt != 2 ||
		r.Status != store.RunStatusFinished || !strings.Contains(r.PortExecution.Invocations["@render[0]"].RecoveryDecision, "external ledger") {
		t.Fatalf("killed attempt did not recover exactly once: calls=%d run=%+v", replayCalls.Load(), r)
	}
}
