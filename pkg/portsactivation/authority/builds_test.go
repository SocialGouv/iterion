package authority

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

func TestAuthorityProtectedBuildsRefuseDisconnectedOldHolder(t *testing.T) {
	record := staticFixture(t)
	static, err := analyzeStaticWithParser(t.Context(), record,
		func(context.Context, natsconfig.Sources) (*natsconfig.Result, error) {
			return parsedStaticFixture(false), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := CorroborateProtectedBuilds(record, static)
	if err != nil || len(approved.Bindings) != 1 || approved.Bindings[0].HolderID != "worker-1" {
		t.Fatalf("tested queue holder was not recognized: %+v %v", approved, err)
	}
	old := record.Holders[0]
	old.ID = "old-disconnected-runner"
	old.ImageDigest = "iterion/runner@sha256:" + strings.Repeat("d", 64)
	old.BuildDigest = strings.Repeat("e", 64)
	record.Holders = append(record.Holders, old)
	record.Credentials[0].HolderIDs = append(record.Credentials[0].HolderIDs, old.ID)
	if _, err := CorroborateProtectedBuilds(record, static); err == nil {
		t.Fatal("a disconnected old build retaining the worker credential was accepted")
	}
	record.BuildApprovals = append(record.BuildApprovals, BuildApproval{ImageDigest: old.ImageDigest,
		BuildDigest: old.BuildDigest, CapabilityDigest: strings.Repeat("f", 64),
		QueueVersion: record.BuildApprovals[0].QueueVersion})
	approved, err = CorroborateProtectedBuilds(record, static)
	if err != nil || len(approved.Bindings) != 2 ||
		approved.Bindings[0].CapabilityDigest == approved.Bindings[1].CapabilityDigest {
		t.Fatalf("two explicitly approved compatible builds could not coexist: %+v %v", approved, err)
	}
	record.BuildApprovals[1].BuildDigest = strings.Repeat("0", 64)
	if _, err := CorroborateProtectedBuilds(record, static); err == nil {
		t.Fatal("a build approval for another binary authorized the old holder")
	}
}
