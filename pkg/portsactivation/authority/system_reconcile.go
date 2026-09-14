package authority

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
	natsclient "github.com/nats-io/nats.go"
	"golang.org/x/sync/errgroup"
)

// SystemCorroboration binds the declared brokers to per-server system-account
// observations. It does not discover brokers omitted by the operator inventory,
// prove disconnected credential custody, or establish observation freshness.
type SystemCorroboration struct {
	Brokers     []ObservedBroker `json:"brokers"`
	connections map[string]map[uint64]observedPrincipal
}

type observedPrincipal struct {
	account string
	user    string
}

type ObservedClientBinding struct {
	ServerID  string `json:"server_id"`
	ClientID  uint64 `json:"client_id"`
	Account   string `json:"account"`
	Principal string `json:"principal"`
}

type ObservedBroker struct {
	ServerID            string `json:"server_id"`
	ConfigDigest        string `json:"config_digest"`
	ObservedConnections int    `json:"observed_connections"`
}

// ObserveAndCorroborateSystem reads the declared broker set and every named
// broker through one authenticated system-account connection. It binds the
// PING.IDZ census to per-broker VARZ/CONNZ and the previously parsed static
// sources. This is read-only corroboration, not an activation proof: a silent
// omitted broker or a disconnected credential holder still requires the
// trusted operator inventory and Kubernetes/credential checks.
func ObserveAndCorroborateSystem(ctx context.Context, nc *natsclient.Conn,
	record *Record, static *StaticAnalysis) (*SystemCorroboration, error) {
	if record == nil || record.validate() != nil || static == nil || len(static.Brokers) != len(record.Brokers) {
		return nil, fmt.Errorf("NATS live observation requires a complete declared inventory and static analysis")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	expected := make([]natsconfig.SystemBrokerIdentity, len(record.Brokers))
	digests := make(map[string]string, len(static.Brokers))
	for _, broker := range static.Brokers {
		if broker.ServerID == "" || digests[broker.ServerID] != "" {
			return nil, fmt.Errorf("NATS static broker inventory is ambiguous")
		}
		digests[broker.ServerID] = broker.ConfigDigest
	}
	for i, broker := range record.Brokers {
		if digests[broker.ServerID] == "" {
			return nil, fmt.Errorf("NATS static broker inventory is incomplete")
		}
		expected[i] = natsconfig.SystemBrokerIdentity{ServerID: broker.ServerID, ServerName: broker.ServerName}
	}
	if _, err := natsconfig.ObserveSystemBrokerSet(probeCtx, nc, expected); err != nil {
		return nil, err
	}
	observations := make([]natsconfig.SystemObservation, len(record.Brokers))
	group, observationCtx := errgroup.WithContext(probeCtx)
	for i, broker := range record.Brokers {
		i, broker := i, broker
		group.Go(func() error {
			observed, err := natsconfig.ObserveSystemServer(observationCtx, nc, broker.ServerID, digests[broker.ServerID])
			if err != nil {
				return fmt.Errorf("NATS broker %s could not be corroborated: %w", broker.ServerID, err)
			}
			observations[i] = *observed
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	result, err := CorroborateSystemObservations(record, static, observations)
	if err != nil {
		return nil, err
	}
	if _, err := bindObservedConnection(result, nc, record.Queue.SystemAccount); err != nil {
		return nil, fmt.Errorf("NATS system observation is not bound to the declared authority account")
	}
	return result, nil
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
	result := &SystemCorroboration{Brokers: make([]ObservedBroker, 0, len(observations)),
		connections: make(map[string]map[uint64]observedPrincipal, len(observations))}
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
		result.connections[observed.ServerID] = make(map[uint64]observedPrincipal, len(observed.Connections))
		for _, connection := range observed.Connections {
			key := [3]string{observed.ServerID, connection.Account, connection.User}
			if connection.CID == 0 || connectionIDs[connection.CID] || !identities[key] {
				return nil, fmt.Errorf("NATS broker has an unidentified or uninventoried connection")
			}
			connectionIDs[connection.CID] = true
			result.connections[observed.ServerID][connection.CID] = observedPrincipal{connection.Account, connection.User}
		}
		result.Brokers = append(result.Brokers, ObservedBroker{ServerID: observed.ServerID,
			ConfigDigest: observed.ConfigDigest, ObservedConnections: len(observed.Connections)})
	}
	slices.SortFunc(result.Brokers, func(a, b ObservedBroker) int { return strings.Compare(a.ServerID, b.ServerID) })
	return result, nil
}

// BindQueueConnection locates this exact live queue client in the corroborated
// broker CONNZ snapshot. The broker's server/client IDs establish account and
// principal identity; the client-supplied connection name is not consulted.
// Reconnection or a client absent from the bounded snapshot refuses the bind.
func BindQueueConnection(record *Record, system *SystemCorroboration,
	queueConnection *natsclient.Conn) (*ObservedClientBinding, error) {
	if record == nil || record.validate() != nil {
		return nil, fmt.Errorf("NATS queue connection has no valid authority record")
	}
	binding, err := bindObservedConnection(system, queueConnection, record.Queue.Account)
	if err != nil {
		return nil, err
	}
	knownBroker, knownPrincipal := false, false
	for _, broker := range record.Brokers {
		knownBroker = knownBroker || broker.ServerID == binding.ServerID
	}
	for _, credential := range record.Credentials {
		knownPrincipal = knownPrincipal || credential.Account == binding.Account &&
			credential.Identity == binding.Principal
	}
	if !knownBroker || !knownPrincipal {
		return nil, fmt.Errorf("NATS queue connection is outside declared broker or principal custody")
	}
	return binding, nil
}

func bindObservedConnection(system *SystemCorroboration, connection *natsclient.Conn,
	expectedAccount string) (*ObservedClientBinding, error) {
	if system == nil || connection == nil || !connection.IsConnected() || expectedAccount == "" {
		return nil, fmt.Errorf("NATS client identity cannot be observed")
	}
	serverID := connection.ConnectedServerId()
	clientID, err := connection.GetClientID()
	if err != nil || serverID == "" || clientID == 0 {
		return nil, fmt.Errorf("NATS client identity is unavailable")
	}
	principal, found := system.connections[serverID][clientID]
	if !found || principal.account != expectedAccount || principal.user == "" ||
		!connection.IsConnected() || connection.ConnectedServerId() != serverID {
		return nil, fmt.Errorf("NATS client account or connection changed during observation")
	}
	latestID, err := connection.GetClientID()
	if err != nil || latestID != clientID {
		return nil, fmt.Errorf("NATS client changed during observation")
	}
	return &ObservedClientBinding{ServerID: serverID, ClientID: clientID,
		Account: principal.account, Principal: principal.user}, nil
}
