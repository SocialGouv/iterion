package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInfo_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	info := Info{
		ID:                     "gpt-5.4",
		Model:                  "gpt-5.4",
		DisplayName:            "GPT-5.4",
		Description:            "Latest GPT-5.4 model",
		IsDefault:              true,
		Hidden:                 false,
		DefaultReasoningEffort: "medium",
		SupportedReasoningEfforts: []ReasoningEffortOption{
			{Value: "low", Label: "Low effort"},
			{Value: "medium", Label: "Medium effort"},
			{Value: "high", Label: "High effort"},
		},
		InputModalities:     []string{"text", "image"},
		SupportsPersonality: false,
		Metadata: map[string]any{
			"upgrade": "gpt-5.4-pro",
		},
	}

	data, err := json.Marshal(info)
	require.NoError(t, err)

	var decoded Info

	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	assert.Equal(t, info, decoded)
}

func TestInfo_JSONFields(t *testing.T) {
	t.Parallel()

	raw := `{
		"id": "gpt-5.4",
		"model": "gpt-5.4",
		"displayName": "GPT-5.4",
		"description": "Full-size model",
		"isDefault": false,
		"hidden": true,
		"defaultReasoningEffort": "high",
		"supportedReasoningEfforts": [{"reasoningEffort": "high", "description": "High effort"}],
		"inputModalities": ["text"],
		"supportsPersonality": true,
		"upgrade": "gpt-5.4-pro"
	}`

	var info Info

	err := json.Unmarshal([]byte(raw), &info)
	require.NoError(t, err)

	assert.Equal(t, "gpt-5.4", info.ID)
	assert.Equal(t, "GPT-5.4", info.DisplayName)
	assert.True(t, info.Hidden)
	assert.True(t, info.SupportsPersonality)
	assert.Equal(t, "high", info.DefaultReasoningEffort)
	assert.Len(t, info.SupportedReasoningEfforts, 1)
	assert.Equal(t, ReasoningEffortOption{Value: "high", Label: "High effort"}, info.SupportedReasoningEfforts[0])
	assert.Equal(t, []string{"text"}, info.InputModalities)
	assert.Equal(t, map[string]any{"upgrade": "gpt-5.4-pro"}, info.Metadata)
}

func TestReasoningEffortOption_UnmarshalCurrentFields(t *testing.T) {
	t.Parallel()

	raw := `{"reasoningEffort":"medium","description":"Medium effort"}`

	var option ReasoningEffortOption

	err := json.Unmarshal([]byte(raw), &option)
	require.NoError(t, err)
	assert.Equal(t, ReasoningEffortOption{Value: "medium", Label: "Medium effort"}, option)
}

func TestListResponse_EmptyModels(t *testing.T) {
	t.Parallel()

	raw := `{"models": []}`

	var resp ListResponse

	err := json.Unmarshal([]byte(raw), &resp)
	require.NoError(t, err)
	assert.Empty(t, resp.Models)
}

func TestListResponse_MultipleModels(t *testing.T) {
	t.Parallel()

	raw := `{
		"models": [
			{"id": "gpt-5.4", "model": "gpt-5.4", "displayName": "GPT-5.4", "upgrade": "gpt-5.4-pro"},
			{"id": "o4-mini", "model": "o4-mini", "displayName": "O4 Mini"}
		],
		"nextCursor": "cursor_123"
	}`

	var resp ListResponse

	err := json.Unmarshal([]byte(raw), &resp)
	require.NoError(t, err)
	require.Len(t, resp.Models, 2)
	assert.Equal(t, "gpt-5.4", resp.Models[0].ID)
	assert.Equal(t, "o4-mini", resp.Models[1].ID)
	assert.Equal(t, map[string]any{"upgrade": "gpt-5.4-pro"}, resp.Models[0].Metadata)
	assert.Equal(t, map[string]any{"nextCursor": "cursor_123"}, resp.Metadata)
}
