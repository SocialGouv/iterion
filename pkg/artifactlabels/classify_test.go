package artifactlabels

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		want []string
	}{
		{"nil", nil, nil},
		{"empty", map[string]any{}, nil},
		{"plain", map[string]any{"foo": "bar", "n": 3}, nil},
		{"plan via plan field", map[string]any{"plan": "## Steps\n1. do"}, []string{LabelPlan}},
		{"plan via text field", map[string]any{"text": "the plan body"}, []string{LabelPlan}},
		{"empty plan string is not plan-shaped", map[string]any{"plan": ""}, nil},
		{"verdict via approved", map[string]any{"approved": true}, []string{LabelVerdict}},
		{"verdict via blockers", map[string]any{"blockers": []any{"x"}}, []string{LabelVerdict}},
		{"verdict via decision string", map[string]any{"decision": "reject"}, []string{LabelVerdict}},
		// Independent detection: a verdict that also carries a prose body is
		// labelled both, mirroring the studio rendering both cards.
		{"both verdict and plan", map[string]any{"approved": false, "plan": "fix it"}, []string{LabelVerdict, LabelPlan}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.data)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Classify(%v) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}
