package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnforceAdditionalProperties(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input map[string]any
		check func(t *testing.T, m map[string]any)
	}{
		{
			name: "sets on root object",
			input: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()
				assert.Equal(t, false, m["additionalProperties"])
			},
		},
		{
			name: "preserves existing value",
			input: map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()
				assert.Equal(t, true, m["additionalProperties"])
			},
		},
		{
			name: "sets on nested object in properties",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"data_profile": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]any{"type": "string"},
						},
					},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()
				assert.Equal(t, false, m["additionalProperties"])

				props, ok := m["properties"].(map[string]any)
				require.True(t, ok)

				dp, ok := props["data_profile"].(map[string]any)
				require.True(t, ok)

				assert.Equal(t, false, dp["additionalProperties"])
			},
		},
		{
			name: "sets on deeply nested objects",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"level1": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"level2": map[string]any{
								"type":       "object",
								"properties": map[string]any{},
							},
						},
					},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				props, ok := m["properties"].(map[string]any)
				require.True(t, ok)

				l1, ok := props["level1"].(map[string]any)
				require.True(t, ok)

				l1Props, ok := l1["properties"].(map[string]any)
				require.True(t, ok)

				l2, ok := l1Props["level2"].(map[string]any)
				require.True(t, ok)

				assert.Equal(t, false, l1["additionalProperties"])
				assert.Equal(t, false, l2["additionalProperties"])
			},
		},
		{
			name: "handles array items",
			input: map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				items, ok := m["items"].(map[string]any)
				require.True(t, ok)

				assert.Equal(t, false, items["additionalProperties"])
			},
		},
		{
			name: "handles anyOf",
			input: map[string]any{
				"anyOf": []any{
					map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
					map[string]any{"type": "string"},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				anyOf, ok := m["anyOf"].([]any)
				require.True(t, ok)

				obj, ok := anyOf[0].(map[string]any)
				require.True(t, ok)

				assert.Equal(t, false, obj["additionalProperties"])
			},
		},
		{
			name: "handles $defs",
			input: map[string]any{
				"type": "object",
				"$defs": map[string]any{
					"Inner": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				defs, ok := m["$defs"].(map[string]any)
				require.True(t, ok)

				inner, ok := defs["Inner"].(map[string]any)
				require.True(t, ok)

				assert.Equal(t, false, inner["additionalProperties"])
			},
		},
		{
			name: "skips non-object types",
			input: map[string]any{
				"type": "string",
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				_, has := m["additionalProperties"]
				assert.False(t, has)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			EnforceAdditionalProperties(tt.input)
			tt.check(t, tt.input)
		})
	}
}

func TestEnforceStrictMode_Required(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input map[string]any
		check func(t *testing.T, m map[string]any)
	}{
		{
			name: "adds missing required with all property keys",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":           map[string]any{"type": "string"},
					"has_clickhouse": map[string]any{"type": "boolean"},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				req, ok := m["required"].([]any)
				require.True(t, ok)

				assert.Contains(t, req, "name")
				assert.Contains(t, req, "has_clickhouse")
				assert.Len(t, req, 2)
			},
		},
		{
			name: "appends missing keys to existing required",
			input: map[string]any{
				"type":     "object",
				"required": []any{"name"},
				"properties": map[string]any{
					"name":           map[string]any{"type": "string"},
					"has_clickhouse": map[string]any{"type": "boolean"},
					"region":         map[string]any{"type": "string"},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				req, ok := m["required"].([]any)
				require.True(t, ok)

				assert.Contains(t, req, "name")
				assert.Contains(t, req, "has_clickhouse")
				assert.Contains(t, req, "region")
				assert.Len(t, req, 3)
			},
		},
		{
			name: "no-op when required already complete",
			input: map[string]any{
				"type":     "object",
				"required": []any{"a", "b"},
				"properties": map[string]any{
					"a": map[string]any{"type": "string"},
					"b": map[string]any{"type": "string"},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				req, ok := m["required"].([]any)
				require.True(t, ok)

				assert.Equal(t, []any{"a", "b"}, req)
			},
		},
		{
			name: "sets required on nested objects",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"data_profile": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"has_clickhouse": map[string]any{"type": "boolean"},
							"has_prometheus": map[string]any{"type": "boolean"},
						},
					},
				},
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				props, ok := m["properties"].(map[string]any)
				require.True(t, ok)

				dp, ok := props["data_profile"].(map[string]any)
				require.True(t, ok)

				req, ok := dp["required"].([]any)
				require.True(t, ok)

				assert.Contains(t, req, "has_clickhouse")
				assert.Contains(t, req, "has_prometheus")
			},
		},
		{
			name: "skips objects without properties",
			input: map[string]any{
				"type": "object",
			},
			check: func(t *testing.T, m map[string]any) {
				t.Helper()

				_, has := m["required"]
				assert.False(t, has)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			EnforceStrictMode(tt.input)
			tt.check(t, tt.input)
		})
	}
}
