package natsconfig

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	natsclient "github.com/nats-io/nats.go"
)

const systemConnLimit = 4096
const systemResponseLimit = 1 << 20

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
