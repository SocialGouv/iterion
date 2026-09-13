package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/internal/s3test"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	storemongo "github.com/SocialGouv/iterion/pkg/store/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type portsTestStoreFactory func(*testing.T) store.RunStore

func runPortsEngineSuite(t *testing.T, factory portsTestStoreFactory) {
	for _, test := range []struct {
		name string
		run  func(*testing.T, portsTestStoreFactory)
	}{
		{"MapZeroOneMany", testPortsEngineMapZeroOneMany},
		{"MapCollectsInInputOrder", testPortsEngineMapCollectsInInputOrder},
		{"JoinWaitsForCommittedProducers", testPortsEngineJoinWaitsForCommittedProducers},
		{"RejectsMapLimitBeforeCreatingItems", testPortsEngineRejectsMapLimitBeforeCreatingItems},
		{"StrictFailureCancelsSiblingsAndStopsAdmission", testPortsEngineStrictFailureCancelsSiblingsAndStopsAdmission},
		{"InvalidOutputBlocksConsumer", testPortsEngineInvalidOutputBlocksConsumer},
		{"ResumeReusesOnlyCommittedWork", testPortsEngineResumeReusesOnlyCommittedWork},
		{"ResumeSourceChangeInvalidatesAffectedDescendants", testPortsEngineResumeSourceChangeInvalidatesAffectedDescendants},
		{"PauseAndResumeKeepsMappedResults", testPortsEnginePauseAndResumeKeepsMappedResults},
		{"OperatorCancelWinsAgainstFinalization", testPortsEngineOperatorCancelWinsAgainstFinalization},
		{"CancelPreventsFurtherAdmissionWithoutContextSignal", testPortsEngineCancelPreventsFurtherAdmissionWithoutContextSignal},
		{"ResumeCannotChangeInterpreterWithForce", testPortsEngineResumeCannotChangeInterpreterWithForce},
		{"ConnectedOptionalWaitsWithoutDefault", testPortsEngineConnectedOptionalWaitsWithoutDefault},
		{"ReservesRootIterationBudget", testPortsEngineReservesRootIterationBudget},
		{"ExactNumericInputs", testPortsEngineExactNumericInputs},
		{"UncertainEffectRequiresAttemptDecision", testPortsEngineUncertainEffectRequiresAttemptDecision},
		{"IdempotentEffectCanResume", testPortsEngineIdempotentEffectCanResume},
		{"InterruptedCommitRecovery", testPortsEngineInterruptedCommitRecovery},
		{"CrossedDAG", testPortsEngineCrossedDAG},
		{"CorrectsProducerBeforePublication", testPortsEngineCorrectsProducerBeforePublication},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, factory) })
	}
}

func TestPortsEngineFilesystem(t *testing.T) { runPortsEngineSuite(t, tmpStore) }

func TestPortsEngineMongo(t *testing.T) {
	if os.Getenv("ITERION_TEST_MONGO_URI") == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("required Engine Mongo tests need ITERION_TEST_MONGO_URI and a writable replica set")
		}
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	runPortsEngineSuite(t, portsTestMongoStore)
}

func TestPortsEngineDefaultOffFilesystem(t *testing.T) { testPortsEngineDefaultOff(t, tmpStore) }

func TestPortsEngineDefaultOffMongo(t *testing.T) {
	if os.Getenv("ITERION_TEST_MONGO_URI") == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("required default-off Mongo test needs a replica set")
		}
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	testPortsEngineDefaultOff(t, portsTestMongoStore)
}

func testPortsEngineDefaultOff(t *testing.T, factory portsTestStoreFactory) {
	s := factory(t)
	var executed atomic.Bool
	engine := New(portsTestWorkflow(t, portsMapSource), s, portsExecutorFunc(func(context.Context, ir.Node, map[string]any) (map[string]any, error) {
		executed.Store(true)
		return nil, errors.New("unactivated native work reached executor")
	}), WithSandboxOverride("none"))
	ctx := portsTestContext(t)
	if err := engine.Run(ctx, "pc1_default_off", map[string]any{"items": []any{"a"}}); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("native run was not gated: %v", err)
	}
	if executed.Load() {
		t.Fatal("unactivated native work reached executor")
	}
	if _, err := s.LoadRun(ctx, "pc1_default_off"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("default-off gate persisted a run: %v", err)
	}
}

func portsTestMongoStore(t *testing.T) store.RunStore {
	t.Helper()
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	_, gateway := s3test.New(t, "native-engine")
	objects, err := blob.NewS3(ctx, blob.Config{Bucket: "native-engine", Region: "us-east-1", Endpoint: gateway.URL, UsePathStyle: true, AccessKeyID: "fixture", SecretAccessKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "runfiles")
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch + "-ports-v1"); err != nil {
			t.Error(err)
		}
		if err := os.RemoveAll(scratch + "-ports-v1-publications"); err != nil {
			t.Error(err)
		}
	})
	s, err := storemongo.New(ctx, storemongo.Config{URI: os.Getenv("ITERION_TEST_MONGO_URI"), Database: "iterion_ports_engine_" + bson.NewObjectID().Hex(), Blob: objects, RunFilesScratchDir: scratch})
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
	var hello struct {
		Writable bool   `bson:"isWritablePrimary"`
		SetName  string `bson:"setName"`
	}
	if err := s.DB().RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil || !hello.Writable || hello.SetName == "" {
		t.Fatalf("Engine tests need a writable replica set: %+v %v", hello, err)
	}
	return s
}
