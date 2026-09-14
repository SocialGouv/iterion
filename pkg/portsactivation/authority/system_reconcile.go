package authority

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

// SystemCorroboration binds the declared brokers to per-server system-account
// observations. It does not discover brokers omitted by the operator inventory,
// prove disconnected credential custody, or establish observation freshness.
type SystemCorroboration struct {
	Brokers []ObservedBroker `json:"brokers"`
}

type ObservedBroker struct {
	ServerID            string `json:"server_id"`
	ConfigDigest        string `json:"config_digest"`
	ObservedConnections int    `json:"observed_connections"`
}

// CorroborateSystemObservations rejects any missing, duplicate, unexpected or
// inconsistent per-broker observation. ObserveSystemServer must collect each
// input through an authenticated system-account connection; a caller-supplied
// value is only corroborating evidence, never an activation proof by itself.
func CorroborateSystemObservations(record *Record, static *StaticAnalysis,
	observations []natsconfig.SystemObservation) (*SystemCorroboration, error) {
	if record == nil || record.validate() != nil || static == nil ||
		len(static.Brokers) != len(record.Brokers) || len(observations) != len(record.Brokers) ||
		len(static.Access) != len(record.Credentials)*len(record.Brokers) {
		return nil, fmt.Errorf("NATS system observations do not cover the declared broker inventory")
	}
	brokers := make(map[string]Broker, len(record.Brokers))
	digests := make(map[string]string, len(static.Brokers))
	identities := make(map[[3]string]bool, len(static.Access))
	for _, broker := range record.Brokers {
		brokers[broker.ServerID] = broker
	}
	for _, broker := range static.Brokers {
		if brokers[broker.ServerID].ServerID == "" || digests[broker.ServerID] != "" ||
			!strings.HasPrefix(broker.ConfigDigest, "sha256:") || len(broker.ConfigDigest) != 71 {
			return nil, fmt.Errorf("NATS static analysis has an incomplete broker identity")
		}
		digests[broker.ServerID] = broker.ConfigDigest
	}
	for _, access := range static.Access {
		key := [3]string{access.ServerID, access.Account, access.Identity}
		knownCredential := false
		for _, credential := range record.Credentials {
			if credential.Account == access.Account && credential.Identity == access.Identity {
				knownCredential = true
				break
			}
		}
		if digests[access.ServerID] == "" || access.Account == "" || access.Identity == "" || identities[key] {
			return nil, fmt.Errorf("NATS static analysis has an incomplete principal inventory")
		}
		if !knownCredential {
			return nil, fmt.Errorf("NATS static analysis has an uninventoried principal")
		}
		identities[key] = true
	}
	result := &SystemCorroboration{Brokers: make([]ObservedBroker, 0, len(observations))}
	seen := make(map[string]bool, len(observations))
	for _, observed := range observations {
		broker := brokers[observed.ServerID]
		if broker.ServerID == "" || seen[observed.ServerID] || observed.ServerName != broker.ServerName ||
			observed.Version != natsconfig.ParserVersion || observed.ConfigDigest != digests[observed.ServerID] ||
			len(observed.Connections) > 4096 {
			return nil, fmt.Errorf("NATS system observation disagrees with a declared broker")
		}
		seen[observed.ServerID] = true
		connectionIDs := make(map[uint64]bool, len(observed.Connections))
		for _, connection := range observed.Connections {
			key := [3]string{observed.ServerID, connection.Account, connection.User}
			if connection.CID == 0 || connectionIDs[connection.CID] || !identities[key] {
				return nil, fmt.Errorf("NATS broker has an unidentified or uninventoried connection")
			}
			connectionIDs[connection.CID] = true
		}
		result.Brokers = append(result.Brokers, ObservedBroker{ServerID: observed.ServerID,
			ConfigDigest: observed.ConfigDigest, ObservedConnections: len(observed.Connections)})
	}
	slices.SortFunc(result.Brokers, func(a, b ObservedBroker) int { return strings.Compare(a.ServerID, b.ServerID) })
	return result, nil
}
