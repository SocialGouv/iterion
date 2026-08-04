package operatormcp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
)

// localBoardPrefix namespaces the boardops tool set inside the operator
// server (the raw names stay reserved for the run-internal __mcp-board
// server).
const localBoardPrefix = "local_board_"

// boardRoot resolves the native board store under a run-store dir —
// the same <store-dir>/dispatcher convention as `iterion issue` and
// the __mcp-board default.
func boardRoot(storeDir string) string {
	return filepath.Join(storeDir, "dispatcher")
}

// localBoardTools wraps the shared boardops tool set with every
// capability granted: the operator drives their own board, so the
// per-node capability gating that protects bot runs does not apply.
func localBoardTools() []Tool {
	caps := boardops.NewCapabilities(strings.Join(boardops.AllCapabilities(), ","))
	boardTools := boardops.ToolsFor(caps)
	out := make([]Tool, 0, len(boardTools))
	for _, bt := range boardTools {
		name := bt.Name
		out = append(out, Tool{
			Name:        localBoardPrefix + name,
			Description: bt.Description + " Operates on the LOCAL native kanban board of this store.",
			InputSchema: bt.InputSchema,
			ReadOnly:    bt.Capability == boardops.CapBoardRead,
			handler: func(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
				st, err := s.board()
				if err != nil {
					return "", false, err
				}
				res, err := boardops.CallWithEnv(st, caps, name, raw, boardops.CallEnv{})
				if err != nil {
					// Board-level failures (unknown state, validation)
					// are tool errors the LLM can recover from; a denied
					// capability cannot happen here (all granted) but
					// would be a real bug worth surfacing loudly.
					if errors.Is(err, boardops.ErrCapabilityDenied) {
						return "", false, err
					}
					return err.Error(), true, nil
				}
				return string(res), false, nil
			},
		})
	}
	return out
}
