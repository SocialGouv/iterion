package authority

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
)

func staticFixture(t *testing.T) *Record {
	t.Helper()
	fixture := validRecordFixture(t)
	fixture["credentials"] = append(fixture["credentials"].([]any), map[string]any{
		"account": "SYS", "identity": "sys", "holder_ids": []string{"authority-1"}, "issuer_ids": []string{"operator"},
	})
	fixture["holders"] = append(fixture["holders"].([]any), map[string]any{
		"id": "authority-1", "kind": "kubernetes", "namespace": "trusted",
		"workload_kind": "Deployment", "workload_name": "iterion-server", "container": "server",
		"service_account": "iterion-server", "image_digest": "iterion/server@sha256:" + strings.Repeat("a", 64),
		"build_digest": strings.Repeat("b", 64), "access_scope": "authority",
		"credential_ref": "trusted/nats-system:NATS_URL",
	})
	record, err := parseFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func parsedStaticFixture(wildcard bool) *natsconfig.Result {
	workerPublish := fmt.Sprintf(`"$JS.API.CONSUMER.MSG.NEXT.%s.%s"`, queue.StreamRuns, queue.ConsumerRunners)
	if wildcard {
		workerPublish = `"$JS.API.>"`
	}
	accounts := fmt.Sprintf(`{"SYS":{"users":[{"user":"sys","password":"private-system","permissions":{"publish":["$SYS.REQ.>"],"subscribe":["_INBOX.>"]}}]},"WORK":{"users":[{"user":"worker-user","password":"private-worker","permissions":{"publish":[%s,"$JS.ACK.>"],"subscribe":["_INBOX.>"]}}]}}`, workerPublish)
	return &natsconfig.Result{ParserVersion: natsconfig.ParserVersion, Digest: "sha256:" + strings.Repeat("a", 64),
		Config: map[string]json.RawMessage{"jetstream": json.RawMessage(`true`),
			"system_account": json.RawMessage(`"SYS"`), "accounts": json.RawMessage(accounts)}}
}

func reorderedStaticFixture(t *testing.T) *natsconfig.Result {
	t.Helper()
	result := parsedStaticFixture(false)
	var accounts map[string]any
	if err := json.Unmarshal(result.Config["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	worker := accounts["WORK"].(map[string]any)["users"].([]any)[0].(map[string]any)
	publish := worker["permissions"].(map[string]any)["publish"].([]any)
	publish[0], publish[1] = publish[1], publish[0]
	encoded, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	result.Config["accounts"] = encoded
	return result
}

func TestAuthorityStaticAnalysisReconcilesEveryConfiguredPrincipal(t *testing.T) {
	record := staticFixture(t)
	analysis, err := analyzeStaticWithParser(t.Context(), record,
		func(context.Context, natsconfig.Sources) (*natsconfig.Result, error) {
			return parsedStaticFixture(false), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.Brokers) != 1 || len(analysis.Access) != 2 || analysis.Brokers[0].ServerID != "NC123" {
		t.Fatalf("static inventory was not reconciled: %+v", analysis)
	}
	for _, access := range analysis.Access {
		if len(access.Exposures) == 0 {
			t.Fatalf("principal %s lost its protected access classification", access.Identity)
		}
	}
	encoded, err := json.Marshal(analysis)
	if err != nil || strings.Contains(string(encoded), "private-worker") || strings.Contains(string(encoded), "private-system") {
		t.Fatal("static analysis exposed NATS credential material")
	}
}

func TestAuthorityStaticAnalysisRefusesUninventoriedOrPrivilegedAccess(t *testing.T) {
	parse := func(context.Context, natsconfig.Sources) (*natsconfig.Result, error) {
		return parsedStaticFixture(false), nil
	}
	t.Run("inventoried identity differs", func(t *testing.T) {
		record := staticFixture(t)
		record.Credentials[0].Identity = "phantom"
		if _, err := analyzeStaticWithParser(t.Context(), record, parse); err == nil {
			t.Fatal("configured NATS principal absent from custody was accepted")
		}
	})
	t.Run("system credential outside authority", func(t *testing.T) {
		record := staticFixture(t)
		record.Holders[1].AccessScope = "runner"
		if _, err := analyzeStaticWithParser(t.Context(), record, parse); err == nil {
			t.Fatal("system account credential held by runner was accepted")
		}
	})
	t.Run("unreviewed JetStream API", func(t *testing.T) {
		record := staticFixture(t)
		if _, err := analyzeStaticWithParser(t.Context(), record,
			func(context.Context, natsconfig.Sources) (*natsconfig.Result, error) {
				return parsedStaticFixture(true), nil
			}); err == nil {
			t.Fatal("wildcard JetStream management permission was accepted")
		}
	})
	t.Run("source parser error is private", func(t *testing.T) {
		record := staticFixture(t)
		_, err := analyzeStaticWithParser(t.Context(), record,
			func(context.Context, natsconfig.Sources) (*natsconfig.Result, error) {
				return nil, errors.New("private-worker")
			})
		if err == nil || strings.Contains(err.Error(), "private-worker") {
			t.Fatalf("NATS parser credential leaked: %v", err)
		}
	})
}

func TestAuthorityStaticAnalysisRejectsBrokerPermissionDrift(t *testing.T) {
	record := staticFixture(t)
	second := record.Brokers[0]
	second.ServerID = "NC456"
	second.ServerName = "nats-1"
	second.PodName = "nats-1"
	second.sources = second.Sources()
	second.sources.Files["main.conf"] = "different"
	record.Brokers = append(record.Brokers, second)
	_, err := analyzeStaticWithParser(t.Context(), record,
		func(_ context.Context, sources natsconfig.Sources) (*natsconfig.Result, error) {
			return parsedStaticFixture(sources.Files["main.conf"] == "different"), nil
		})
	if err == nil {
		t.Fatal("brokers with different effective protected permissions were accepted")
	}
}

func TestAuthorityStaticAnalysisIgnoresEquivalentACLRuleOrder(t *testing.T) {
	record := staticFixture(t)
	second := record.Brokers[0]
	second.ServerID = "NC456"
	second.ServerName = "nats-1"
	second.PodName = "nats-1"
	second.sources = second.Sources()
	second.sources.Files["main.conf"] = "same permissions, opposite rule order"
	record.Brokers = append(record.Brokers, second)
	reordered := reorderedStaticFixture(t)
	analysis, err := analyzeStaticWithParser(t.Context(), record,
		func(_ context.Context, sources natsconfig.Sources) (*natsconfig.Result, error) {
			if sources.Files["main.conf"] == "same permissions, opposite rule order" {
				return reordered, nil
			}
			return parsedStaticFixture(false), nil
		})
	if err != nil || len(analysis.Brokers) != 2 {
		t.Fatalf("equivalent reordered ACLs were rejected: %v", err)
	}
}

func TestAuthorityStaticAnalysisParsesBrokersConcurrentlyInStableOrder(t *testing.T) {
	record := staticFixture(t)
	second := record.Brokers[0]
	second.ServerID, second.ServerName, second.PodName = "NC456", "nats-1", "nats-1"
	second.sources = second.Sources()
	second.sources.Files["main.conf"] = "second broker"
	record.Brokers = append(record.Brokers, second)
	var started atomic.Int32
	bothStarted := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	analysis, err := analyzeStaticWithParser(ctx, record,
		func(ctx context.Context, _ natsconfig.Sources) (*natsconfig.Result, error) {
			if started.Add(1) == 2 {
				close(bothStarted)
			}
			select {
			case <-bothStarted:
				return parsedStaticFixture(false), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	if err != nil || len(analysis.Brokers) != 2 || started.Load() != 2 {
		t.Fatalf("independent broker analyses did not overlap: %+v %v", analysis, err)
	}
	_, err = analyzeStaticWithParser(t.Context(), record,
		func(_ context.Context, _ natsconfig.Sources) (*natsconfig.Result, error) {
			return nil, errors.New("private parser failure")
		})
	if err == nil || !strings.Contains(err.Error(), "NC123") || strings.Contains(err.Error(), "private parser failure") {
		t.Fatalf("parallel parser failures lost stable redacted broker order: %v", err)
	}
}
