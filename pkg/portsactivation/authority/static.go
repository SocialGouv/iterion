package authority

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

// StaticAnalysis is a credential-free projection of operator sources. It is
// neither a live-broker observation nor a distributed activation proof.
type StaticAnalysis struct {
	Brokers []StaticBroker `json:"brokers"`
	Access  []StaticAccess `json:"access"`
}

type StaticBroker struct {
	ServerID     string `json:"server_id"`
	ConfigDigest string `json:"config_digest"`
}

type StaticAccess struct {
	ServerID  string                      `json:"server_id"`
	Account   string                      `json:"account"`
	Identity  string                      `json:"identity"`
	Kind      string                      `json:"kind"`
	Exposures []natsconfig.AccessExposure `json:"exposures"`
}

// AnalyzeStatic parses every declared broker source with the pinned NATS
// parser, reconciles every configured principal with named custody entries,
// and classifies protected subject access. Its caller must still compare the
// digests with actual VARZ, inspect pod arguments and credentials, establish
// complete deployment scope and enforce compatible build exclusion.
func AnalyzeStatic(ctx context.Context, record *Record) (*StaticAnalysis, error) {
	return analyzeStaticWithParser(ctx, record, natsconfig.Parse)
}

func analyzeStaticWithParser(ctx context.Context, record *Record,
	parse func(context.Context, natsconfig.Sources) (*natsconfig.Result, error)) (*StaticAnalysis, error) {
	if record == nil || record.validate() != nil || parse == nil {
		return nil, fmt.Errorf("NATS static authority requires a valid operator record and pinned parser")
	}
	credentials := make(map[[2]string]CredentialCustody, len(record.Credentials))
	for _, entry := range record.Credentials {
		credentials[[2]string{entry.Account, entry.Identity}] = entry
	}
	holders := make(map[string]CredentialHolder, len(record.Holders))
	for _, holder := range record.Holders {
		holders[holder.ID] = holder
	}
	analysis := &StaticAnalysis{}
	var baseline []natsconfig.Principal
	profiles := make([]*natsconfig.Profile, len(record.Brokers))
	parseErrors := make([]error, len(record.Brokers))
	var parsing sync.WaitGroup
	for index, broker := range record.Brokers {
		index, broker := index, broker
		parsing.Add(1)
		go func() {
			defer parsing.Done()
			result, err := parse(ctx, broker.Sources())
			if err != nil {
				parseErrors[index] = fmt.Errorf("NATS authority source for broker %s could not be parsed", broker.ServerID)
				return
			}
			profile, err := natsconfig.LoadProfile(result)
			if err != nil {
				parseErrors[index] = fmt.Errorf("NATS authority source for broker %s has unsupported authorization: %w", broker.ServerID, err)
				return
			}
			profiles[index] = profile
		}()
	}
	parsing.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for index, broker := range record.Brokers {
		if parseErrors[index] != nil {
			return nil, parseErrors[index]
		}
		profile := profiles[index]
		if profile.SystemAccount != record.Queue.SystemAccount || !slices.Contains(profile.Accounts, record.Queue.Account) ||
			len(profile.Principals) != len(credentials) {
			return nil, fmt.Errorf("NATS authority source disagrees with declared account or credential custody")
		}
		if index == 0 {
			baseline = normalizedPrincipals(profile.Principals)
		} else if !reflect.DeepEqual(normalizedPrincipals(profile.Principals), baseline) {
			return nil, fmt.Errorf("NATS authority brokers have inconsistent effective principal permissions")
		}
		analysis.Brokers = append(analysis.Brokers, StaticBroker{ServerID: broker.ServerID, ConfigDigest: profile.ConfigDigest})
		for _, principal := range profile.Principals {
			entry, ok := credentials[[2]string{principal.Account, principal.Identity}]
			if !ok {
				return nil, fmt.Errorf("NATS authority configuration contains an uninventoried principal")
			}
			exposures, err := natsconfig.AnalyzeProtectedAccess(principal, record.Queue)
			if err != nil {
				return nil, fmt.Errorf("NATS authority cannot classify a configured principal: %w", err)
			}
			for _, exposure := range exposures {
				if exposure.Surface == "unreviewed_jetstream_api" {
					return nil, fmt.Errorf("NATS authority principal has unreviewed protected API access")
				}
			}
			if principal.Account == record.Queue.SystemAccount {
				for _, holderID := range entry.HolderIDs {
					if holders[holderID].AccessScope != "authority" {
						return nil, fmt.Errorf("NATS system account credential is held outside the named authority")
					}
				}
			}
			analysis.Access = append(analysis.Access, StaticAccess{ServerID: broker.ServerID,
				Account: principal.Account, Identity: principal.Identity, Kind: principal.Kind, Exposures: exposures})
		}
	}
	slices.SortFunc(analysis.Brokers, func(a, b StaticBroker) int { return strings.Compare(a.ServerID, b.ServerID) })
	slices.SortFunc(analysis.Access, func(a, b StaticAccess) int {
		if c := strings.Compare(a.ServerID, b.ServerID); c != 0 {
			return c
		}
		if c := strings.Compare(a.Account, b.Account); c != 0 {
			return c
		}
		return strings.Compare(a.Identity, b.Identity)
	})
	return analysis, nil
}

func normalizedPrincipals(principals []natsconfig.Principal) []natsconfig.Principal {
	normalized := make([]natsconfig.Principal, len(principals))
	for i, principal := range principals {
		principal.Publish.Allow = normalizedRules(principal.Publish.Allow)
		principal.Publish.Deny = normalizedRules(principal.Publish.Deny)
		principal.Subscribe.Allow = normalizedRules(principal.Subscribe.Allow)
		principal.Subscribe.Deny = normalizedRules(principal.Subscribe.Deny)
		normalized[i] = principal
	}
	return normalized
}

func normalizedRules(rules []string) []string {
	if len(rules) == 0 {
		return nil
	}
	normalized := slices.Clone(rules)
	slices.Sort(normalized)
	return slices.Compact(normalized)
}
