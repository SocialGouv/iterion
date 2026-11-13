package apikit

import (
	"reflect"
	"testing"
)

func TestEffortCapabilities(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		wantSupported    []string
		wantDefault      string
		wantSupportedLen int  // sanity check on length when full slice is too verbose
		wantNotSupported bool // true when reasoning_effort is not supported by this model
	}{
		{
			name:          "opus 4.8 by canonical",
			input:         "claude-opus-4-8",
			wantSupported: []string{"low", "medium", "high", "xhigh", "max"},
			wantDefault:   "high",
		},
		{
			name:          "opus alias resolves to Opus 5.5 (API default medium)",
			input:         "opus",
			wantSupported: []string{"low", "medium", "high", "xhigh", "max"},
			wantDefault:   "medium",
		},
		{
			name:          "opus 4.7 by canonical",
			input:         "claude-opus-4-7",
			wantSupported: []string{"low", "medium", "high", "xhigh", "max"},
			wantDefault:   "xhigh",
		},
		{
			name:          "opus 4.6 has no xhigh",
			input:         "claude-opus-4-6",
			wantSupported: []string{"low", "medium", "high", "max"},
			wantDefault:   "high",
		},
		{
			name:          "sonnet 4.6 has no xhigh",
			input:         "claude-sonnet-4-6",
			wantSupported: []string{"low", "medium", "high", "max"},
			wantDefault:   "high",
		},
		{
			name:             "haiku does not support effort",
			input:            "claude-haiku-4-5",
			wantNotSupported: true,
		},
		{
			name:             "sonnet 4.7 effort not yet documented",
			input:            "claude-sonnet-4-7",
			wantNotSupported: true,
		},
		{
			name:          "openai gpt-5.5",
			input:         "gpt-5.5",
			wantSupported: []string{"minimal", "low", "medium", "high"},
			wantDefault:   "medium",
		},
		{
			name:          "openai openrouter alias",
			input:         "openai/gpt-5.4-mini",
			wantSupported: []string{"minimal", "low", "medium", "high"},
			wantDefault:   "medium",
		},
		{
			name:             "grok no documented matrix",
			input:            "grok-3",
			wantNotSupported: true,
		},
		{
			name:             "unknown model",
			input:            "totally-made-up",
			wantNotSupported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, def := EffortCapabilities(tt.input)
			if tt.wantNotSupported {
				if got != nil || def != "" {
					t.Errorf("EffortCapabilities(%q) = (%v, %q), want (nil, \"\")", tt.input, got, def)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.wantSupported) {
				t.Errorf("EffortCapabilities(%q) supported = %v, want %v", tt.input, got, tt.wantSupported)
			}
			if def != tt.wantDefault {
				t.Errorf("EffortCapabilities(%q) default = %q, want %q", tt.input, def, tt.wantDefault)
			}
		})
	}
}

func TestAnthropicProfile(t *testing.T) {
	tests := []struct {
		input           string
		supportsEffort  bool
		thinkingMode    string
		rejectsSampling bool
	}{
		{"claude-opus-4-8", true, "adaptive", true},
		{"opus", true, "adaptive", true}, // alias → 5
		{"claude-opus-5", true, "adaptive", true},
		{"claude-fable-5-1", true, "adaptive", true},
		{"claude-sonnet-5", true, "adaptive", false},
		{"claude-opus-4-7", true, "adaptive", true},
		{"claude-opus-4-6", true, "adaptive", false},
		{"claude-sonnet-4-6", true, "adaptive", false},
		{"claude-haiku-4-5", false, "", false},
		{"gpt-5.5", true, "", false}, // OpenAI: effort yes, adaptive thinking is Anthropic-only
		{"totally-made-up", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p := AnthropicProfile(tt.input)
			if p.SupportsEffort() != tt.supportsEffort {
				t.Errorf("SupportsEffort() = %v, want %v", p.SupportsEffort(), tt.supportsEffort)
			}
			if p.ThinkingMode != tt.thinkingMode {
				t.Errorf("ThinkingMode = %q, want %q", p.ThinkingMode, tt.thinkingMode)
			}
			if p.RejectsSampling != tt.rejectsSampling {
				t.Errorf("RejectsSampling = %v, want %v", p.RejectsSampling, tt.rejectsSampling)
			}
		})
	}
}

func TestValidateEffortForModel(t *testing.T) {
	tests := []struct {
		level   string
		model   string
		wantErr bool
	}{
		{"", "claude-opus-4-8", false},      // empty = API default
		{"xhigh", "claude-opus-4-8", false}, // supported on 4-8
		{"max", "claude-opus-4-8", false},   // supported on 4-8
		{"xhigh", "claude-opus-4-6", true},  // 4-6 has no xhigh
		{"on", "claude-opus-4-8", true},     // reasoning *mode*, not an effort
		{"stream", "claude-opus-4-8", true}, // ditto
		{"bogus", "claude-opus-4-8", true},  // unknown level
		{"high", "totally-made-up", false},  // unknown model: pass recognised level through
		{"xhigh", "gpt-5.5", true},          // OpenAI has no xhigh
	}
	for _, tt := range tests {
		t.Run(tt.level+"/"+tt.model, func(t *testing.T) {
			err := ValidateEffortForModel(tt.level, tt.model)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateEffortForModel(%q,%q) err=%v, wantErr=%v", tt.level, tt.model, err, tt.wantErr)
			}
		})
	}
}
