package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
)

// The ROUTE screen (#3259): the launch picker must screen the backend the
// run will ACTUALLY take — the inherit route — with the same declared-
// capability questions it asks of every candidate. Before this screen, the
// operator touching no per-node dropdown launched a run the first node
// killed, blessed by a preview that promised it "cannot drift from runtime
// admission". Both refusals are terminal ErrCapabilityUnsupported; saying so
// after the launch is not saying so.

func TestTheRunLevelRouteIsScreenedForCodemode(t *testing.T) {
	t.Setenv("ITERION_PERMISSION", "")
	t.Setenv("ITERION_DEFAULT_BACKEND", "codex") // the env decides the route

	wf := compileSource(t, `agent a:
  model: "anthropic/claude-sonnet-4-6"
  codemode: on

workflow main:
  entry: a
  a -> done
`)
	// runBackend is empty here on purpose: the ROUTE comes from the env
	// default, the layer the per-candidate loop never asks about. The
	// candidates still carry codex, so the OLD half of the screen (per
	// candidate) already barred codex — what was missing is the route.
	options, routeRefusals := buildBackendOverrideOptions(
		wf, "off", "", "", []string{"claw", "codex"},
	)

	if _, ok := options["a"]; !ok {
		t.Fatalf("node a has no screened options")
	}
	if options["a"]["codex"].UnavailableReason == "" {
		t.Errorf("codex is offered without a codemode reason — the candidate screen regressed")
	}
	if options["a"]["claw"].UnavailableReason != "" {
		t.Errorf("claw serves codemode and must not be barred: %q", options["a"]["claw"].UnavailableReason)
	}
	reason, ok := routeRefusals["a"]
	if !ok || reason == "" {
		t.Fatalf("route_refusals is empty for node a: the route the run takes (env default codex) is unscreened — a launch on it dies at the first node, blessed by the preview")
	}
	if !strings.Contains(reason, "codemode") {
		t.Errorf("the route refusal does not name codemode: %q", reason)
	}
}

func TestTheRunLevelRouteIsScreenedForAmbient(t *testing.T) {
	t.Setenv("ITERION_PERMISSION", "")
	t.Setenv("ITERION_DEFAULT_BACKEND", "")
	t.Setenv(ambient.PolicyEnv, "") // the workflow layer decides

	wf := compileSource(t, `agent a:
  model: "anthropic/claude-sonnet-4-6"
  backend: "opencode"
  ambient_context: workspace

workflow main:
  entry: a
  a -> done
`)
	// The route here is the node's own pin (opencode), and the policy is
	// DECLARED on the node: workspace is Refused on opencode (measured, the
	// verdict table in pkg/backend/ambient).
	options, routeRefusals := buildBackendOverrideOptions(
		wf, "off", "", "", []string{"claw", "opencode"},
	)

	if options["a"]["opencode"].UnavailableReason == "" {
		t.Errorf("opencode is offered without an ambient reason on a node declaring ambient_context: workspace — the candidate screen is missing the ambient half")
	}
	// claw HONOURS workspace: neither barred nor route-refused.
	if options["a"]["claw"].UnavailableReason != "" {
		t.Errorf("claw honours workspace and must not be barred: %q", options["a"]["claw"].UnavailableReason)
	}
	reason, ok := routeRefusals["a"]
	if !ok || reason == "" {
		t.Fatalf("route_refusals is empty: a node declaring ambient_context: workspace, routed to opencode, is killed at dispatch and the preview said nothing")
	}
	if !strings.Contains(reason, "ambient_context") {
		t.Errorf("the route refusal does not name ambient_context: %q", reason)
	}
}

func TestAnUndeclaredAmbientDefaultIsNeverRefused(t *testing.T) {
	t.Setenv("ITERION_PERMISSION", "")
	t.Setenv("ITERION_DEFAULT_BACKEND", "")
	t.Setenv(ambient.PolicyEnv, "")

	// NO ambient declaration anywhere: the default (workspace) is applied as
	// far as the backend can, never refused — executor_ambient only refuses
	// a DECLARED policy.
	wf := compileSource(t, `agent a:
  model: "anthropic/claude-sonnet-4-6"
  backend: "opencode"

workflow main:
  entry: a
  a -> done
`)
	options, routeRefusals := buildBackendOverrideOptions(
		wf, "off", "", "", []string{"claw", "opencode"},
	)
	if len(routeRefusals) != 0 {
		t.Errorf("route_refusals = %v, want empty: the default is never refused", routeRefusals)
	}
	if options["a"]["opencode"].UnavailableReason != "" {
		t.Errorf("opencode barred on an undeclared default: %q", options["a"]["opencode"].UnavailableReason)
	}
}

func TestAnUnmeasuredBackendIsNotBarredByAmbient(t *testing.T) {
	t.Setenv("ITERION_PERMISSION", "")
	t.Setenv("ITERION_DEFAULT_BACKEND", "")
	t.Setenv(ambient.PolicyEnv, "")

	// A backend absent from the verdict table is Unmeasured: the dispatch
	// proceeds with a warning, so the picker must not bar it — barring a
	// choice the run would accept is the false refusal this screen exists
	// to avoid.
	wf := compileSource(t, `agent a:
  model: "anthropic/claude-sonnet-4-6"
  ambient_context: workspace

workflow main:
  entry: a
  a -> done
`)
	options, routeRefusals := buildBackendOverrideOptions(
		wf, "off", "", "", []string{"unmeasured-backend"},
	)
	if options["a"]["unmeasured-backend"].UnavailableReason != "" {
		t.Errorf("an Unmeasured backend was barred: %q — the dispatch proceeds with a warning", options["a"]["unmeasured-backend"].UnavailableReason)
	}
	if len(routeRefusals) != 0 {
		t.Errorf("route_refusals = %v, want empty for an Unmeasured backend", routeRefusals)
	}
}
