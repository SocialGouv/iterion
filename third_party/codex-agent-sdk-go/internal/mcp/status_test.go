package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatus_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := Status{
		MCPServers: []ServerStatus{
			{Name: "calc", Status: "connected"},
			{Name: "memory", Status: "disconnected"},
		},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded Status
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, original, decoded)
}
