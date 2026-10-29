package main

// The in-container runner is the SECOND production host of the claw tool set,
// and the one the default path uses: sandboxing is on by default, and
// tool.SandboxPlacementOf puts `agent` in the container — a sandboxed node's
// subagent must reach the container's tools, never the launcher's. Wired only
// on the host, a delegation would work unsandboxed and refuse by default
// (#1940).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// agentProbe is a well-formed `agent` input: claw's own validator accepts it,
// so whatever comes back is the capability's answer, not a complaint the
// model could fix by rewriting its arguments.
const agentProbe = `{"description":"summarise","prompt":"Summarise Go interfaces","subagent_type":"general-purpose","model":"test/absent-provider"}`

// TestTheInContainerAgentToolReachesARunner: the registrar the runner uses in
// production hands the `agent` tool a real runner, so the call lands in a
// child conversation. The probe names a provider this registry cannot
// resolve, so the runner answers with a model-resolution error — proof it
// RAN, where a typed capability refusal would prove it was never wired.
//
// Red when the registrar passes nil: the call then comes back as
// *delegate.ErrCapabilityUnsupported, in-container, on the default path.
func TestTheInContainerAgentToolReachesARunner(t *testing.T) {
	reg := tool.NewRegistry()
	registrar := sandboxLocalToolRegistrar(model.NewRegistry(), model.EventHooks{})
	if err := registrar(reg, t.TempDir()); err != nil {
		t.Fatalf("sandboxLocalToolRegistrar: %v", err)
	}
	td, err := reg.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	out, err := td.Execute(context.Background(), json.RawMessage(agentProbe))
	var unsupported *delegate.ErrCapabilityUnsupported
	if errors.As(err, &unsupported) {
		t.Fatalf("the in-container `agent` tool answers the capability refusal (%v) — no runner is wired "+
			"where the sandbox placement of `agent` says one must be", err)
	}
	if err == nil {
		t.Fatalf("the probe names an unresolvable provider, so a runner must report that; got %q", out)
	}
	if !strings.Contains(err.Error(), "subagent") || !strings.Contains(err.Error(), "resolve model") {
		t.Errorf("the error %q does not read as a runner's own model resolution failure", err)
	}
}

// TestAnUnwiredInContainerAgentToolStillRefusesInTheOpen keeps the other half
// honest: a runner built with no subagent closure registers the tool and
// refuses every call, rather than letting claw's executor report a start.
func TestAnUnwiredInContainerAgentToolStillRefusesInTheOpen(t *testing.T) {
	reg := tool.NewRegistry()
	if err := registerSandboxLocalTools(reg, t.TempDir(), nil); err != nil {
		t.Fatalf("registerSandboxLocalTools: %v", err)
	}
	td, err := reg.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	out, err := td.Execute(context.Background(), json.RawMessage(agentProbe))
	if err == nil {
		t.Fatalf("an unwired `agent` tool answered %q instead of refusing", out)
	}
	if strings.Contains(out, "running") || strings.Contains(out, "agent_id") {
		t.Errorf("the refusal handed the model claw's start metadata: %q", out)
	}
}
