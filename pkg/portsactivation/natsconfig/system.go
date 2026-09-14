package natsconfig

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	natsclient "github.com/nats-io/nats.go"
)

const systemConnLimit = 4096
const systemResponseLimit = 1 << 20
const systemBrokerLimit = 16
const systemPingWindow = 2 * time.Second

// SystemBrokerIdentity is a broker observed through the system account's
// IDZ reply. The operator inventory remains the authority on completeness.
type SystemBrokerIdentity struct {
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
}

// ObserveSystemBrokerSet corroborates the declared broker IDs with a bounded
// multi-response PING.IDZ. It waits for the entire window even after all
// expected replies arrive, so a promptly responding unknown broker is not
// hidden by an early return. A silent or partitioned broker can still be
// absent: this observation alone never proves exhaustive deployment scope.
func ObserveSystemBrokerSet(ctx context.Context, nc *natsclient.Conn, expected []SystemBrokerIdentity) ([]SystemBrokerIdentity, error) {
	if nc == nil || len(expected) == 0 || len(expected) > systemBrokerLimit {
		return nil, fmt.Errorf("NATS system broker census requires a bounded declared inventory")
	}
	opCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	declared := make(map[string]string, len(expected))
	names := make(map[string]bool, len(expected))
	for _, broker := range expected {
		if !profileAccountName(broker.ServerID) || !systemBrokerName(broker.ServerName) ||
			declared[broker.ServerID] != "" || names[broker.ServerName] {
			return nil, fmt.Errorf("NATS system broker census has an invalid declared identity")
		}
		declared[broker.ServerID] = broker.ServerName
		names[broker.ServerName] = true
	}
	if declared[nc.ConnectedServerId()] == "" {
		return nil, fmt.Errorf("NATS system connection is outside the declared broker inventory")
	}
	inbox := nc.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("NATS system broker census could not subscribe: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.FlushWithContext(opCtx); err != nil {
		return nil, fmt.Errorf("NATS system broker census subscription is unavailable: %w", err)
	}
	if err := nc.PublishRequest("$SYS.REQ.SERVER.PING.IDZ", inbox, nil); err != nil {
		return nil, fmt.Errorf("NATS system broker census request was refused: %w", err)
	}
	if err := nc.FlushWithContext(opCtx); err != nil {
		return nil, fmt.Errorf("NATS system broker census request was not sent: %w", err)
	}
	window, cancel := context.WithTimeout(opCtx, systemPingWindow)
	defer cancel()
	seen := make(map[string]bool, len(expected))
	for {
		message, err := sub.NextMsgWithContext(window)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("NATS system broker census was interrupted: %w", ctx.Err())
			}
			if opCtx.Err() != nil {
				return nil, fmt.Errorf("NATS system broker census exceeded its operation deadline: %w", opCtx.Err())
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, natsclient.ErrTimeout) {
				break
			}
			return nil, fmt.Errorf("NATS system broker census replies are unavailable: %w", err)
		}
		if err := recordSystemBrokerReply(message.Data, declared, seen); err != nil {
			return nil, err
		}
	}
	if len(seen) != len(expected) {
		return nil, fmt.Errorf("NATS system broker census missed a declared broker")
	}
	result := slices.Clone(expected)
	slices.SortFunc(result, func(a, b SystemBrokerIdentity) int { return strings.Compare(a.ServerID, b.ServerID) })
	return result, nil
}

func recordSystemBrokerReply(raw []byte, declared map[string]string, seen map[string]bool) error {
	if len(raw) == 0 || len(raw) > 1024 {
		return fmt.Errorf("NATS system broker IDZ response is missing or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("NATS system broker IDZ response has an unsupported shape")
	}
	fields := make(map[string]string, 3)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || key != "id" && key != "name" && key != "host" {
			return fmt.Errorf("NATS system broker IDZ response has an unsupported field")
		}
		if _, duplicate := fields[key]; duplicate {
			return fmt.Errorf("NATS system broker IDZ response repeats an identity field")
		}
		var value string
		if decoder.Decode(&value) != nil {
			return fmt.Errorf("NATS system broker IDZ response has an invalid identity value")
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF ||
		!profileAccountName(fields["id"]) || !systemBrokerName(fields["name"]) || fields["host"] == "" ||
		declared[fields["id"]] != fields["name"] || seen[fields["id"]] {
		return fmt.Errorf("NATS system broker IDZ response disagrees with the declared inventory")
	}
	seen[fields["id"]] = true
	return nil
}

// Match the authority record's bounded broker-name alphabet. Server IDs
// remain NATS-generated tokens; names may include dots, slashes and colons.
func systemBrokerName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '/' || c == ':') {
			return false
		}
	}
	return true
}

// SystemConnection is a current broker observation. It can contradict a
// closed operator inventory, but disconnected credential holders do not
// appear here and must be accounted for by the trusted operator source.
type SystemConnection struct {
	CID     uint64 `json:"cid"`
	Account string `json:"account"`
	User    string `json:"user"`
	Name    string `json:"name"`
}

type SystemObservation struct {
	ServerID     string             `json:"server_id"`
	ServerName   string             `json:"server_name"`
	Version      string             `json:"version"`
	ConfigDigest string             `json:"config_digest"`
	Connections  []SystemConnection `json:"connections"`
}

type systemEnvelope[T any] struct {
	Server *systemServerInfo `json:"server"`
	Data   *T                `json:"data"`
	Error  json.RawMessage   `json:"error"`
}

type systemServerInfo struct {
	ID string `json:"id"`
}

type systemConnzEntry struct {
	CID     uint64 `json:"cid"`
	Account string `json:"account"`
	User    string `json:"authorized_user"`
	Name    string `json:"name"`
}

type systemConnz struct {
	ID       string             `json:"server_id"`
	Total    int                `json:"total"`
	Offset   int                `json:"offset"`
	NumConns int                `json:"num_connections"`
	Conns    []systemConnzEntry `json:"connections"`
}

// The pinned broker constructs one CONNZ response from a single snapshot.
// Offset pagination cannot be made a complete census by comparing totals:
// a disconnect and reconnect between pages can keep the total unchanged
// while shifting a continuously connected holder across the boundary.
func completeSystemConnections(serverID string, response *systemEnvelope[systemConnz]) ([]SystemConnection, error) {
	if response == nil || response.Server == nil || response.Data == nil || response.Server.ID != serverID {
		return nil, fmt.Errorf("NATS system CONNZ response has an inconsistent broker identity")
	}
	page := response.Data
	if page.ID != serverID || page.Total < 0 || page.Total > systemConnLimit || page.Offset != 0 ||
		page.NumConns != page.Total || len(page.Conns) != page.Total {
		return nil, fmt.Errorf("NATS system CONNZ did not return one complete bounded snapshot")
	}
	seen := make(map[uint64]bool, page.Total)
	connections := make([]SystemConnection, 0, page.Total)
	for _, connection := range page.Conns {
		if connection.CID == 0 || connection.Account == "" || connection.User == "" || seen[connection.CID] {
			return nil, fmt.Errorf("NATS system CONNZ has an unidentified or duplicate connection")
		}
		seen[connection.CID] = true
		connections = append(connections, SystemConnection{
			CID: connection.CID, Account: connection.Account, User: connection.User, Name: connection.Name,
		})
	}
	return connections, nil
}

func systemRequest[T any](ctx context.Context, nc *natsclient.Conn, subject string, request any) (*systemEnvelope[T], error) {
	var payload []byte
	if request != nil {
		var err error
		payload, err = json.Marshal(request)
		if err != nil {
			return nil, fmt.Errorf("NATS system request could not be encoded")
		}
	}
	message, err := nc.RequestWithContext(ctx, subject, payload)
	if err != nil {
		return nil, fmt.Errorf("NATS system observation unavailable: %w", err)
	}
	if len(message.Data) == 0 || len(message.Data) > systemResponseLimit {
		return nil, fmt.Errorf("NATS system response exceeds supported size")
	}
	var response systemEnvelope[T]
	if err := json.Unmarshal(message.Data, &response); err != nil || response.Server == nil || response.Server.ID == "" ||
		response.Data == nil || len(response.Error) != 0 && string(response.Error) != "null" {
		return nil, fmt.Errorf("NATS system response is incomplete or refused")
	}
	return &response, nil
}

// ObserveSystemServer reads one explicitly identified broker through an
// already authenticated system-account connection. The caller must separately
// establish that its target inventory includes every participating broker;
// requesting known IDs alone cannot discover an omitted server.
func ObserveSystemServer(ctx context.Context, nc *natsclient.Conn, serverID, expectedDigest string) (*SystemObservation, error) {
	if nc == nil || !profileAccountName(serverID) || len(expectedDigest) != 71 ||
		!strings.HasPrefix(expectedDigest, "sha256:") {
		return nil, fmt.Errorf("NATS system observation requires a named broker and expected configuration digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(expectedDigest, "sha256:")); err != nil {
		return nil, fmt.Errorf("NATS system observation requires a valid configuration digest")
	}
	varz, err := systemRequest[struct {
		ID           string `json:"server_id"`
		Name         string `json:"server_name"`
		Version      string `json:"version"`
		ConfigDigest string `json:"config_digest"`
		AuthRequired bool   `json:"auth_required"`
	}](ctx, nc, "$SYS.REQ.SERVER."+serverID+".VARZ", nil)
	if err != nil {
		return nil, err
	}
	if varz.Server.ID != serverID || varz.Data.ID != serverID || varz.Data.Name == "" ||
		varz.Data.Version != ParserVersion || varz.Data.ConfigDigest != expectedDigest || !varz.Data.AuthRequired {
		return nil, fmt.Errorf("NATS system VARZ disagrees with the pinned authorized broker")
	}
	result := &SystemObservation{ServerID: serverID, ServerName: varz.Data.Name,
		Version: varz.Data.Version, ConfigDigest: varz.Data.ConfigDigest}
	connz, err := systemRequest[systemConnz](ctx, nc, "$SYS.REQ.SERVER."+serverID+".CONNZ", struct {
		Auth  bool `json:"auth"`
		Limit int  `json:"limit"`
	}{true, systemConnLimit})
	if err != nil {
		return nil, err
	}
	result.Connections, err = completeSystemConnections(serverID, connz)
	if err != nil {
		return nil, err
	}
	return result, nil
}
