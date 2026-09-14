package authority

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

func systemCorroborationFixture(t *testing.T) (*Record, *StaticAnalysis, []natsconfig.SystemObservation) {
	t.Helper()
	record := staticFixture(t)
	static, err := analyzeStaticWithParser(t.Context(), record,
		func(context.Context, natsconfig.Sources) (*natsconfig.Result, error) {
			return parsedStaticFixture(false), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return record, static, []natsconfig.SystemObservation{{
		ServerID: "NC123", ServerName: "nats-0", Version: natsconfig.ParserVersion,
		ConfigDigest: "sha256:" + strings.Repeat("a", 64),
		Connections: []natsconfig.SystemConnection{
			{CID: 1, Account: "SYS", User: "sys"},
			{CID: 2, Account: "WORK", User: "worker-user"},
		},
	}}
}

func TestNATSSystemCorroborationBindsEveryDeclaredBrokerAndPrincipal(t *testing.T) {
	record, static, observations := systemCorroborationFixture(t)
	second := record.Brokers[0]
	second.ServerID, second.ServerName, second.PodName = "NC456", "nats-1", "nats-1"
	record.Brokers = append(record.Brokers, second)
	static.Brokers = append(static.Brokers, StaticBroker{ServerID: "NC456", ConfigDigest: observations[0].ConfigDigest})
	for _, access := range static.Access {
		access.ServerID = "NC456"
		static.Access = append(static.Access, access)
	}
	other := observations[0]
	other.ServerID, other.ServerName = "NC456", "nats-1"
	observations = append(observations, other)
	result, err := CorroborateSystemObservations(record, static, observations)
	if err != nil || len(result.Brokers) != 2 || result.Brokers[0].ServerID != "NC123" ||
		result.Brokers[1].ServerID != "NC456" || result.Brokers[0].ObservedConnections != 2 {
		t.Fatalf("declared NATS broker was not corroborated: %+v %v", result, err)
	}
}

func TestNATSSystemCorroborationRefusesMissingForeignOrStaleBrokerEvidence(t *testing.T) {
	tests := map[string]func(*Record, *StaticAnalysis, *[]natsconfig.SystemObservation){
		"missing broker":  func(_ *Record, _ *StaticAnalysis, o *[]natsconfig.SystemObservation) { *o = nil },
		"extra broker":    func(_ *Record, _ *StaticAnalysis, o *[]natsconfig.SystemObservation) { *o = append(*o, (*o)[0]) },
		"wrong server ID": func(_ *Record, _ *StaticAnalysis, o *[]natsconfig.SystemObservation) { (*o)[0].ServerID = "foreign" },
		"wrong name":      func(_ *Record, _ *StaticAnalysis, o *[]natsconfig.SystemObservation) { (*o)[0].ServerName = "foreign" },
		"stale digest": func(_ *Record, _ *StaticAnalysis, o *[]natsconfig.SystemObservation) {
			(*o)[0].ConfigDigest = "sha256:" + strings.Repeat("b", 64)
		},
		"unreviewed version": func(_ *Record, _ *StaticAnalysis, o *[]natsconfig.SystemObservation) { (*o)[0].Version = "0.0.0" },
		"unknown principal": func(_ *Record, _ *StaticAnalysis, o *[]natsconfig.SystemObservation) {
			(*o)[0].Connections[0].User = "unknown"
		},
		"duplicate connection":  func(_ *Record, _ *StaticAnalysis, o *[]natsconfig.SystemObservation) { (*o)[0].Connections[1].CID = 1 },
		"missing static broker": func(_ *Record, s *StaticAnalysis, _ *[]natsconfig.SystemObservation) { s.Brokers = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record, static, observations := systemCorroborationFixture(t)
			mutate(record, static, &observations)
			if _, err := CorroborateSystemObservations(record, static, observations); err == nil {
				t.Fatal("missing or contradictory NATS broker evidence was accepted")
			}
		})
	}
}
