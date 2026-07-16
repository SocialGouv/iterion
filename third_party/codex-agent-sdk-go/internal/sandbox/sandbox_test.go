package sandbox

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettings_JSONOmitEmpty(t *testing.T) {
	t.Parallel()

	settings := Settings{}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.JSONEq(t, "{}", string(data))
}

func TestSettings_JSONWithNestedValues(t *testing.T) {
	t.Parallel()

	enabled := true
	allowLocalBinding := true
	httpPort := 8080
	settings := Settings{
		Enabled: &enabled,
		Network: &NetworkConfig{
			AllowLocalBinding: &allowLocalBinding,
			HTTPProxyPort:     &httpPort,
			AllowUnixSockets:  []string{"/tmp/socket"},
		},
		IgnoreViolations: &IgnoreViolations{
			File:    []string{"open:/tmp"},
			Network: []string{"connect:localhost"},
		},
	}

	data, err := json.Marshal(settings)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, true, got["enabled"])

	network, ok := got["network"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, network["allowLocalBinding"])
	require.Equal(t, float64(8080), network["httpProxyPort"])
	require.Equal(t, []any{"/tmp/socket"}, network["allowUnixSockets"])

	ignore, ok := got["ignoreViolations"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{"open:/tmp"}, ignore["file"])
	require.Equal(t, []any{"connect:localhost"}, ignore["network"])
}
