package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRegisterClawRunToolsCapabilityGateAndExecution(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.CreateRun(context.Background(), "run-1", "demo", nil); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	if err := RegisterClawRunTools(reg, &RunConfig{Store: rs, Capabilities: []string{runops.CapRunsRead}}); err != nil {
		t.Fatal(err)
	}
	definition, err := reg.Resolve("mcp__iterion_runs__run_get")
	if err != nil {
		t.Fatal(err)
	}
	out, err := definition.Execute(context.Background(), []byte(`{"run_id":"run-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"id":"run-1"`) {
		t.Fatalf("run_get output = %s", out)
	}
}
