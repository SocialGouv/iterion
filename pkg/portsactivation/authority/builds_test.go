package authority

import (
	"context"
	"encoding/json"
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

func TestAuthorityProtectedBuildsRefusePublishOnlyOldHolder(t *testing.T) {
	record := staticFixture(t)
	parsed := parsedStaticFixture(false)
	var accounts map[string]any
	if err := json.Unmarshal(parsed.Config["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	worker := accounts["WORK"].(map[string]any)["users"].([]any)[0].(map[string]any)
	worker["permissions"] = map[string]any{
		"publish": []string{record.Queue.RunSubject}, "subscribe": []string{"_INBOX.>"},
	}
	encoded, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Config["accounts"] = encoded
	static, err := analyzeStaticWithParser(t.Context(), record,
		func(context.Context, natsconfig.Sources) (*natsconfig.Result, error) { return parsed, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CorroborateProtectedBuilds(record, static); err != nil {
		t.Fatalf("approved publish-only producer was rejected: %v", err)
	}
	record.Holders[0].ImageDigest = "iterion/old-server@sha256:" + strings.Repeat("d", 64)
	record.Holders[0].BuildDigest = strings.Repeat("e", 64)
	if _, err := CorroborateProtectedBuilds(record, static); err == nil {
		t.Fatal("an unapproved publisher could still inject messages into the shared run stream")
	}
}
