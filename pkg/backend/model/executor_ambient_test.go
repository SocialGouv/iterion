package model

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestAmbientContextPolicyWarnsOnlyWhenAChosenValueGoesUnenforced(t *testing.T) {
	cases := []struct {
		name     string
		node     string
		backend  string
		want     ambient.Policy
		warnWant bool
	}{
		{"chosen value on a backend that ignores it", "none", "opencode", ambient.None, true},
		{"chosen value on an enforcing backend", "none", "claude_code", ambient.None, false},
		{"the default on a backend that ignores it", "", "kimi", ambient.Workspace, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := &ClawExecutor{logger: iterlog.New(iterlog.LevelInfo, &buf)}
			got := e.ambientContextPolicy(backendFields{id: "n", ambientContext: c.node}, c.backend)
			if got != c.want {
				t.Errorf("policy = %v, want %v", got, c.want)
			}
			if warned := strings.Contains(buf.String(), "is not enforced on backend"); warned != c.warnWant {
				t.Errorf("warned = %v, want %v (log: %q)", warned, c.warnWant, buf.String())
			}
		})
	}
}
