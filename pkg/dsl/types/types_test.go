package types

import "testing"

func TestMCPTransportString(t *testing.T) {
	tests := []struct {
		name string
		mt   MCPTransport
		want string
	}{
		{"unknown zero value", MCPTransportUnknown, "unknown"},
		{"stdio", MCPTransportStdio, "stdio"},
		{"http", MCPTransportHTTP, "http"},
		{"sse", MCPTransportSSE, "sse"},
		{"out of range", MCPTransport(99), "unknown"},
		{"negative", MCPTransport(-1), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mt.String(); got != tt.want {
				t.Errorf("MCPTransport(%d).String() = %q, want %q", int(tt.mt), got, tt.want)
			}
		})
	}
}

func TestFieldTypeString(t *testing.T) {
	tests := []struct {
		name string
		ft   FieldType
		want string
	}{
		{"string zero value", FieldTypeString, "string"},
		{"bool", FieldTypeBool, "bool"},
		{"int", FieldTypeInt, "int"},
		{"float", FieldTypeFloat, "float"},
		{"json", FieldTypeJSON, "json"},
		{"string array", FieldTypeStringArray, "string[]"},
		{"out of range", FieldType(99), "unknown"},
		{"negative", FieldType(-1), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ft.String(); got != tt.want {
				t.Errorf("FieldType(%d).String() = %q, want %q", int(tt.ft), got, tt.want)
			}
		})
	}
}

func TestSessionModeString(t *testing.T) {
	tests := []struct {
		name string
		sm   SessionMode
		want string
	}{
		{"fresh zero value", SessionFresh, "fresh"},
		{"inherit", SessionInherit, "inherit"},
		{"inherit if available", SessionInheritIfAvailable, "inherit_if_available"},
		{"artifacts only", SessionArtifactsOnly, "artifacts_only"},
		{"fork", SessionFork, "fork"},
		{"persist", SessionPersist, "persist"},
		{"out of range", SessionMode(99), "unknown"},
		{"negative", SessionMode(-1), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sm.String(); got != tt.want {
				t.Errorf("SessionMode(%d).String() = %q, want %q", int(tt.sm), got, tt.want)
			}
		})
	}
}

func TestRouterModeString(t *testing.T) {
	tests := []struct {
		name string
		rm   RouterMode
		want string
	}{
		{"fan out all zero value", RouterFanOutAll, "fan_out_all"},
		{"condition", RouterCondition, "condition"},
		{"round robin", RouterRoundRobin, "round_robin"},
		{"llm", RouterLLM, "llm"},
		{"fan out each", RouterFanOutEach, "fan_out_each"},
		{"out of range", RouterMode(99), "unknown"},
		{"negative", RouterMode(-1), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rm.String(); got != tt.want {
				t.Errorf("RouterMode(%d).String() = %q, want %q", int(tt.rm), got, tt.want)
			}
		})
	}
}

func TestAwaitModeString(t *testing.T) {
	tests := []struct {
		name string
		am   AwaitMode
		want string
	}{
		{"none zero value", AwaitNone, "none"},
		{"wait all", AwaitWaitAll, "wait_all"},
		{"best effort", AwaitBestEffort, "best_effort"},
		{"out of range", AwaitMode(99), "unknown"},
		{"negative", AwaitMode(-1), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.am.String(); got != tt.want {
				t.Errorf("AwaitMode(%d).String() = %q, want %q", int(tt.am), got, tt.want)
			}
		})
	}
}

func TestInteractionModeString(t *testing.T) {
	tests := []struct {
		name string
		im   InteractionMode
		want string
	}{
		{"none zero value", InteractionNone, "none"},
		{"human", InteractionHuman, "human"},
		{"llm", InteractionLLM, "llm"},
		{"llm or human", InteractionLLMOrHuman, "llm_or_human"},
		{"review", InteractionReview, "review"},
		{"out of range", InteractionMode(99), "unknown"},
		{"negative", InteractionMode(-1), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.im.String(); got != tt.want {
				t.Errorf("InteractionMode(%d).String() = %q, want %q", int(tt.im), got, tt.want)
			}
		})
	}
}
