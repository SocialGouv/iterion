package natsconfig

import (
	"fmt"
	"slices"
	"testing"

	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
)

func protectedFixture() QueueTopology {
	return QueueTopology{
		Account: "WORK", SystemAccount: "SYS", Stream: queue.StreamRuns, Consumer: queue.ConsumerRunners,
		DLQStream: queue.StreamRunsDLQ, RunSubject: queue.SubjectRuns, DLQSubject: queue.SubjectRunsDLQ,
		LockBucket: queue.KVRunLocks, RolloutBucket: queue.KVRolloutEpochs,
	}
}

func hasExposure(exposures []AccessExposure, surface string) bool {
	return slices.ContainsFunc(exposures, func(exposure AccessExposure) bool { return exposure.Surface == surface })
}

func TestNATSProtectedAccessClassifiesWholePermissionLanguages(t *testing.T) {
	topology := protectedFixture()
	for _, tc := range []struct {
		name      string
		principal Principal
		want      []string
		refuse    []string
	}{
		{"unrelated grant", Principal{Account: "WORK", Identity: "safe", Publish: SubjectPermissions{Allow: []string{"safe.>"}}, Subscribe: SubjectPermissions{Allow: []string{"safe.>"}}}, nil, []string{"jetstream_api", "queue_messages"}},
		{"exact fetch and reply", Principal{Account: "WORK", Identity: "runner", Publish: SubjectPermissions{Allow: []string{"$JS.API.CONSUMER.MSG.NEXT.ITERION_RUNS.iterion-runners"}}, Subscribe: SubjectPermissions{Allow: []string{"_INBOX.>"}}}, []string{"jetstream_api", "jetstream_reply"}, []string{"unreviewed_jetstream_api", "acknowledgment"}},
		{"unknown API variant", Principal{Account: "WORK", Identity: "broad", Publish: SubjectPermissions{Allow: []string{"$JS.API.SERVER.REMOVE"}}}, []string{"jetstream_api", "unreviewed_jetstream_api"}, nil},
		{"acknowledgment", Principal{Account: "WORK", Identity: "acker", Publish: SubjectPermissions{Allow: []string{"$JS.ACK.>"}}}, []string{"acknowledgment"}, []string{"jetstream_api"}},
		{"reply injection", Principal{Account: "WORK", Identity: "injector", Publish: SubjectPermissions{Allow: []string{"_INBOX.>"}}, Subscribe: SubjectPermissions{Allow: []string{"$JS.API.>"}}}, []string{"jetstream_request_visibility", "jetstream_reply_injection"}, []string{"queue_messages", "unreviewed_jetstream_api"}},
		{"direct run subscription", Principal{Account: "WORK", Identity: "reader", Publish: SubjectPermissions{Allow: []string{"safe.>"}}, Subscribe: SubjectPermissions{Allow: []string{"iterion.queue.>"}}}, []string{"queue_messages"}, []string{"jetstream_api"}},
		{"KV backing-stream mutation", Principal{Account: "WORK", Identity: "mutator", Publish: SubjectPermissions{Allow: []string{"$JS.API.STREAM.PURGE.KV_iterion-runner-rollout"}}}, []string{"jetstream_api"}, []string{"unreviewed_jetstream_api"}},
		{"KV core write", Principal{Account: "WORK", Identity: "kv", Publish: SubjectPermissions{Allow: []string{"$KV.iterion-runner-rollout.>"}}}, []string{"queue_kv"}, []string{"jetstream_api"}},
		{"native cancellation", Principal{Account: "WORK", Identity: "canceller", Publish: SubjectPermissions{Allow: []string{"iterion.cancel.*"}}}, []string{"native_control"}, []string{"jetstream_api"}},
		{"native steering", Principal{Account: "WORK", Identity: "steerer", Subscribe: SubjectPermissions{Allow: []string{"iterion.steer.>"}}, Publish: SubjectPermissions{Allow: []string{"safe.>"}}}, []string{"native_control"}, []string{"jetstream_api"}},
		{"system authority", Principal{Account: "SYS", Identity: "sys"}, []string{"system_authority"}, []string{"jetstream_api", "queue_messages", "acknowledgment"}},
		{"account isolation", Principal{Account: "OTHER", Identity: "other"}, nil, []string{"system_authority", "jetstream_api", "queue_messages", "acknowledgment"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exposures, err := AnalyzeProtectedAccess(tc.principal, topology)
			if err != nil {
				t.Fatal(err)
			}
			for _, surface := range tc.want {
				if !hasExposure(exposures, surface) {
					t.Errorf("expected %s exposure: %+v", surface, exposures)
				}
			}
			for _, surface := range tc.refuse {
				if hasExposure(exposures, surface) {
					t.Errorf("unexpected %s exposure: %+v", surface, exposures)
				}
			}
			for _, exposure := range exposures {
				if exposure.Witness == "" {
					t.Errorf("exposure had no concrete subject: %+v", exposure)
				}
			}
		})
	}
}

func TestNATSProtectedAccessHonorsConfiguredDenyBudget(t *testing.T) {
	for _, count := range []int{46, 128} {
		denies := make([]string, count)
		for i := range denies {
			denies[i] = fmt.Sprintf("unrelated.%d", i)
		}
		principal := Principal{Account: "WORK", Identity: "restricted", Publish: SubjectPermissions{
			Allow: []string{"$JS.API.CONSUMER.MSG.NEXT.ITERION_RUNS.iterion-runners"}, Deny: denies,
		}, Subscribe: SubjectPermissions{Allow: []string{"safe.>"}}}
		exposures, err := AnalyzeProtectedAccess(principal, protectedFixture())
		if err != nil || !hasExposure(exposures, "jetstream_api") || hasExposure(exposures, "unreviewed_jetstream_api") {
			t.Fatalf("%d configured denies must not be displaced by catalog rules: %+v %v", count, exposures, err)
		}
	}
}

func TestNATSProtectedAccessRefusesUnboundTopology(t *testing.T) {
	topology := protectedFixture()
	for _, mutate := range []func(*QueueTopology){
		func(q *QueueTopology) { q.Account = "" },
		func(q *QueueTopology) { q.SystemAccount = q.Account },
		func(q *QueueTopology) { q.Consumer = "runner.*" },
		func(q *QueueTopology) { q.RunSubject = "iterion.queue.>" },
		func(q *QueueTopology) { q.DLQStream = q.Stream },
		func(q *QueueTopology) { q.RolloutBucket = q.LockBucket },
	} {
		candidate := topology
		mutate(&candidate)
		if _, err := AnalyzeProtectedAccess(Principal{Account: "WORK", Identity: "runner"}, candidate); err == nil {
			t.Fatalf("unsupported queue topology accepted: %+v", candidate)
		}
	}
}
