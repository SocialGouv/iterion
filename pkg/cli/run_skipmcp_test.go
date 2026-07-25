package cli

import "testing"

func TestSkipMCPHealthFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"yes", false}, // only "1" / "true" (any case) are truthy
	}
	for _, c := range cases {
		t.Setenv("ITERION_SKIP_MCP_HEALTH", c.val)
		if got := skipMCPHealthFromEnv(); got != c.want {
			t.Errorf("skipMCPHealthFromEnv() with ITERION_SKIP_MCP_HEALTH=%q = %v, want %v", c.val, got, c.want)
		}
	}
}
