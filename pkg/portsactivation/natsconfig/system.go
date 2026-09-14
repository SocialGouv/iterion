package natsconfig

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	natsclient "github.com/nats-io/nats.go"
)

const systemConnPageSize = 128
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
	Server *struct {
		ID string `json:"id"`
	} `json:"server"`
	Data  *T              `json:"data"`
	Error json.RawMessage `json:"error"`
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
	seen := make(map[uint64]bool)
	total := -1
	for offset := 0; ; offset += systemConnPageSize {
		connz, err := systemRequest[struct {
			ID       string `json:"server_id"`
			Total    int    `json:"total"`
			Offset   int    `json:"offset"`
			NumConns int    `json:"num_connections"`
			Conns    []struct {
				CID     uint64 `json:"cid"`
				Account string `json:"account"`
				User    string `json:"authorized_user"`
				Name    string `json:"name"`
			} `json:"connections"`
		}](ctx, nc, "$SYS.REQ.SERVER."+serverID+".CONNZ", struct {
			Auth   bool `json:"auth"`
			Offset int  `json:"offset"`
			Limit  int  `json:"limit"`
		}{true, offset, systemConnPageSize})
		if err != nil {
			return nil, err
		}
		page := connz.Data
		if connz.Server.ID != serverID || page.ID != serverID || page.Total < 0 || page.Total > systemConnLimit ||
			page.Offset != offset || page.NumConns != len(page.Conns) || len(page.Conns) > systemConnPageSize ||
			(total >= 0 && page.Total != total) {
			return nil, fmt.Errorf("NATS system CONNZ changed or returned incomplete pagination")
		}
		if total < 0 {
			total = page.Total
		}
		for _, connection := range page.Conns {
			if connection.CID == 0 || connection.Account == "" || connection.User == "" || seen[connection.CID] {
				return nil, fmt.Errorf("NATS system CONNZ has an unidentified or duplicate connection")
			}
			seen[connection.CID] = true
			result.Connections = append(result.Connections, SystemConnection{
				CID: connection.CID, Account: connection.Account, User: connection.User, Name: connection.Name,
			})
		}
		if len(result.Connections) == total {
			return result, nil
		}
		if len(page.Conns) != systemConnPageSize || len(result.Connections) > total {
			return nil, fmt.Errorf("NATS system CONNZ omitted current connections")
		}
	}
}
