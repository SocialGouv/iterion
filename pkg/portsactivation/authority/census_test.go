package authority

import (
	"strings"
	"testing"
	"time"

	queuecore "github.com/SocialGouv/iterion/pkg/queue"
	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
)

func censusAuthorityFixture(t *testing.T, now time.Time) (*Record, []queue.PortCapabilityObservation) {
	t.Helper()
	record, err := parseFixture(t, validRecordFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	claim := queue.PortInstanceCapability{
		Version: queue.PortCapabilityVersion, Principal: "worker-user", Instance: "pod-z",
		BuildDigest:      "sha256:" + record.Holders[0].BuildDigest,
		CapabilityDigest: strings.Repeat("a", 64), StoreIdentity: "mongodb:fixture",
		Account: record.Queue.Account, Stream: record.Queue.Stream, Consumer: record.Queue.Consumer,
		QueueVersion: queuecore.SchemaVersion, RunnerEpoch: 7, AuthorityEpoch: record.Epoch,
	}
	return record, []queue.PortCapabilityObservation{{Capability: claim, Revision: 3,
		RecordedAt: now.Add(-5 * time.Second)}}
}

func TestAuthorityCensusCorroboratesNamedBuildAndRefusesStaleOrForeignClaims(t *testing.T) {
	now := time.Now().UTC()
	const backend = "mongodb:fixture"
	record, observations := censusAuthorityFixture(t, now)
	second := observations[0]
	second.Capability.Instance = "pod-a"
	observations = append(observations, second)
	result, err := CorroborateObservedCensus(record, observations, backend, 7, now)
	if err != nil || len(result.Members) != 2 || result.Members[0].Instance != "pod-a" ||
		result.Members[1].HolderID != "worker-1" || result.Members[1].RecordedAt.IsZero() {
		t.Fatalf("known capable instances did not bind to their declared holder: %+v %v", result, err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Record, *[]queue.PortCapabilityObservation)
	}{
		{"unknown principal", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].Capability.Principal = "unknown" }},
		{"old build", func(_ *Record, o *[]queue.PortCapabilityObservation) {
			(*o)[0].Capability.BuildDigest = "sha256:" + strings.Repeat("f", 64)
		}},
		{"wrong capability", func(_ *Record, o *[]queue.PortCapabilityObservation) {
			(*o)[0].Capability.CapabilityDigest = strings.Repeat("f", 64)
		}},
		{"wrong backend", func(_ *Record, o *[]queue.PortCapabilityObservation) {
			(*o)[0].Capability.StoreIdentity = "mongodb:other"
		}},
		{"wrong account", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].Capability.Account = "OTHER" }},
		{"wrong stream", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].Capability.Stream = "OTHER" }},
		{"wrong consumer", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].Capability.Consumer = "OTHER" }},
		{"wrong authority epoch", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].Capability.AuthorityEpoch++ }},
		{"wrong runner epoch", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].Capability.RunnerEpoch++ }},
		{"stale", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].RecordedAt = now.Add(-time.Minute) }},
		{"future", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].RecordedAt = now.Add(time.Second) }},
		{"missing revision", func(_ *Record, o *[]queue.PortCapabilityObservation) { (*o)[0].Revision = 0 }},
		{"duplicate key", func(_ *Record, o *[]queue.PortCapabilityObservation) { *o = append(*o, (*o)[0]) }},
		{"ambiguous shared credential", func(record *Record, _ *[]queue.PortCapabilityObservation) {
			other := record.Holders[0]
			other.ID = "another-holder"
			record.Holders = append(record.Holders, other)
			record.Credentials[0].HolderIDs = append(record.Credentials[0].HolderIDs, other.ID)
		}},
		{"unapproved holder with same build", func(record *Record, _ *[]queue.PortCapabilityObservation) {
			other := record.Holders[0]
			other.ID = "old-image-holder"
			other.ImageDigest = "iterion/old-runner@sha256:" + strings.Repeat("d", 64)
			record.Holders = append(record.Holders, other)
			record.Credentials[0].HolderIDs = append(record.Credentials[0].HolderIDs, other.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, observations := censusAuthorityFixture(t, now)
			tc.mutate(record, &observations)
			if _, err := CorroborateObservedCensus(record, observations, backend, 7, now); err == nil {
				t.Fatal("contradictory or ambiguous capability announcement was accepted")
			}
		})
	}
}

func TestAuthorityCensusAcceptsTwoIndependentlyApprovedBuilds(t *testing.T) {
	now := time.Now().UTC()
	record, observations := censusAuthorityFixture(t, now)
	secondHolder := record.Holders[0]
	secondHolder.ID = "worker-2"
	secondHolder.ImageDigest = "iterion/runner@sha256:" + strings.Repeat("d", 64)
	secondHolder.BuildDigest = strings.Repeat("e", 64)
	record.Holders = append(record.Holders, secondHolder)
	record.Credentials[0].HolderIDs = append(record.Credentials[0].HolderIDs, secondHolder.ID)
	record.BuildApprovals = append(record.BuildApprovals, BuildApproval{
		ImageDigest: secondHolder.ImageDigest, BuildDigest: secondHolder.BuildDigest,
		CapabilityDigest: strings.Repeat("f", 64), QueueVersion: queuecore.SchemaVersion,
	})
	second := observations[0]
	second.Capability.Instance = "pod-a"
	second.Capability.BuildDigest = "sha256:" + secondHolder.BuildDigest
	second.Capability.CapabilityDigest = record.BuildApprovals[1].CapabilityDigest
	observations = append(observations, second)
	result, err := CorroborateObservedCensus(record, observations, "mongodb:fixture", 7, now)
	if err != nil || len(result.Members) != 2 || result.Members[0].HolderID != secondHolder.ID ||
		result.Members[1].HolderID != "worker-1" {
		t.Fatalf("compatible rollout builds were not bound individually: %+v %v", result, err)
	}
}
