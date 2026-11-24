package tool

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	clawlsp "github.com/SocialGouv/iterion/pkg/claw/pkg/api/lsp"
	clawteam "github.com/SocialGouv/iterion/pkg/claw/pkg/api/team"
	clawtools "github.com/SocialGouv/iterion/pkg/claw/pkg/api/tools"
)

// clawNamesNothingConsumes are the claw built-ins whose executors answer
// success into registries no iterion code reads back: cron entries no
// scheduler fires, teams nothing groups by, and an lsp dispatcher with no
// server behind it. Sorted, as ClawRefusedBuiltins returns them.
var clawNamesNothingConsumes = []string{
	"cron_create", "cron_delete", "cron_get", "cron_list",
	"lsp",
	"team_create", "team_delete", "team_get", "team_list",
}

// TestTheRefusedClawCatalogHoldsOnlyNamesTheRegistryServes binds the
// compile-time list to the registry from the side where the registry is real:
// a refusal is for a tool the registry HOLDS (otherwise C135 rejects the name
// and C159 has nothing to add), so every listed name must be a catalog builtin
// and must RESOLVE in the registry every run builds. The list is pinned to
// exactly the nine names, so an entry that leaves it — or one that joins it —
// is a decision this test makes visible.
func TestTheRefusedClawCatalogHoldsOnlyNamesTheRegistryServes(t *testing.T) {
	names := toolcatalog.ClawRefusedBuiltins()
	if strings.Join(names, ",") != strings.Join(clawNamesNothingConsumes, ",") {
		t.Errorf("the refused list is %v; want exactly %v", names, clawNamesNothingConsumes)
	}

	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	for _, name := range names {
		if !toolcatalog.IsBuiltin(name) {
			t.Errorf("%q is listed as refused but is not in the catalog — C159 would stay silent and C135 would reject the name", name)
			continue
		}
		if _, resolveErr := reg.Resolve(name); resolveErr != nil {
			t.Errorf("%q is listed as refused but does not resolve: %v — a refusal is for a tool the registry HOLDS", name, resolveErr)
		}
	}
}

// TestTheRefusedCatalogNamesEveryToolThatAnswersWithoutDoing is the run-time
// half: each of the nine, resolved from the registry every run builds and
// called with an empty input, earns the typed capability refusal carrying the
// headline the ultracode grant conditions on and the catalog's own reason and
// remedy — the words C159 quoted at compile time — and its advertised
// description opens on the same headline. A call that answered instead would
// be the defect: a cron entry nothing fires, a team nothing reads, an lsp
// "dispatched" to no server.
func TestTheRefusedCatalogNamesEveryToolThatAnswersWithoutDoing(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	family := map[string]string{"cron": "schedule", "team": "the next node reads", "lsp": "through bash"}
	for _, name := range clawNamesNothingConsumes {
		td, err := reg.Resolve(name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		refusal, refused := toolcatalog.ClawRefusesBuiltin(name)
		if !refused {
			t.Fatalf("%s is not on the refused list", name)
		}
		if !strings.HasPrefix(td.Description, "The "+name+" tool is "+toolcatalog.ClawRefusedToolHeadline) {
			t.Errorf("%s advertises %q; want it to open on the refusal headline", name, td.Description)
		}
		if !strings.Contains(td.Description, refusal.String()) {
			t.Errorf("%s's description does not carry the refusal %q: %s", name, refusal.String(), td.Description)
		}
		out, execErr := td.Execute(context.Background(), json.RawMessage(`{}`))
		var unsupported *delegate.ErrCapabilityUnsupported
		if !errors.As(execErr, &unsupported) {
			t.Errorf("%s returned (%q, %v); want the typed capability refusal", name, out, execErr)
			continue
		}
		msg := unsupported.Error()
		for _, want := range []string{toolcatalog.ClawRefusedToolHeadline, refusal.Reason, refusal.Remedy} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s's refusal does not carry %q: %s", name, want, msg)
			}
		}
		key, _, _ := strings.Cut(name, "_")
		if !strings.Contains(refusal.Remedy, family[key]) {
			t.Errorf("%s's remedy %q does not point at %q, where the capability is served", name, refusal.Remedy, family[key])
		}
	}
}

// TestARefusedToolRefusesMalformedInputToo: the refusal answers before the
// input is decoded, so a truncated or malformed call to a refused name — one
// of the nine, or `agent` on a host that wires no subagent runner — earns the
// same typed refusal as a well-formed one, never a decode error that sends
// the model rewriting arguments for a tool that cannot run.
func TestARefusedToolRefusesMalformedInputToo(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	for _, name := range append(slices.Clone(clawNamesNothingConsumes), "agent") {
		td, err := reg.Resolve(name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		for _, input := range []string{`{"name":`, `not json`, `[1,2]`} {
			out, execErr := td.Execute(context.Background(), json.RawMessage(input))
			var unsupported *delegate.ErrCapabilityUnsupported
			if !errors.As(execErr, &unsupported) {
				t.Errorf("%s with input %s returned (%q, %v); want the typed capability refusal", name, input, out, execErr)
			}
		}
	}
}

// TestNoHostLiftsTheRefusal: the refusal holds whatever the host wires. A host
// that passes its own Teams, Crons and LSP registries — LSP with a connected
// server, the shape the live e2e host builds — still gets the refusal for all
// nine, and a host that registers its own executor for a listed name through
// RegisterClawTool gets the refusal too, its executor never called. This is
// what lets C159 state the refusal without a host hedge.
func TestNoHostLiftsTheRefusal(t *testing.T) {
	ws := t.TempDir()
	lspReg := clawlsp.NewRegistry()
	lspReg.Register("go", clawlsp.StatusConnected, &ws, nil)
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{
		Workspace: ws,
		Teams:     clawteam.NewTeamRegistry(),
		Crons:     clawteam.NewCronRegistry(),
		LSP:       lspReg,
	}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	inputs := map[string]string{
		"lsp":         `{"action":"diagnostics","path":"probe.go"}`,
		"cron_create": `{"name":"n","schedule":"0 * * * *","prompt":"p"}`,
	}
	for _, name := range clawNamesNothingConsumes {
		td, err := reg.Resolve(name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		input := inputs[name]
		if input == "" {
			input = `{}`
		}
		out, execErr := td.Execute(context.Background(), json.RawMessage(input))
		var unsupported *delegate.ErrCapabilityUnsupported
		if !errors.As(execErr, &unsupported) {
			t.Errorf("%s on a host-wired registry returned (%q, %v); want the typed capability refusal", name, out, execErr)
		}
	}

	hostReg := NewRegistry()
	called := false
	if err := RegisterClawTool(hostReg, clawtools.CronCreateTool(), func(context.Context, map[string]any) (string, error) {
		called = true
		return `{"cron_id":"host"}`, nil
	}); err != nil {
		t.Fatalf("RegisterClawTool: %v", err)
	}
	td, err := hostReg.Resolve("cron_create")
	if err != nil {
		t.Fatalf("resolve cron_create: %v", err)
	}
	out, execErr := td.Execute(context.Background(), json.RawMessage(`{}`))
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(execErr, &unsupported) || called {
		t.Errorf("a host executor for cron_create answered (%q, %v, called=%v); want the typed refusal and the executor never called", out, execErr, called)
	}
}

// TestAnInjectedRefusalReachesTheRegistry drives the chokepoint over a name
// the production list does not hold: a refusal installed for `config` must
// reach the call through registerClawTool like the nine shipped ones, so the
// next entry is refused at run time without a registrar of its own. `config`
// is the probe because its executor is pure: a broken chokepoint answers a
// lookup, never a network call.
func TestAnInjectedRefusalReachesTheRegistry(t *testing.T) {
	const probe = "config"
	if _, refused := toolcatalog.ClawRefusesBuiltin(probe); refused {
		t.Fatalf("%q is refused in production, so the injection would prove nothing", probe)
	}
	restore := toolcatalog.SetClawRefusedBuiltinForTest(probe, toolcatalog.ClawRefusal{
		Reason: "a probe reason", Remedy: "A probe remedy",
	})
	defer restore()

	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	td, err := reg.Resolve(probe)
	if err != nil {
		t.Fatalf("resolve %s: %v", probe, err)
	}
	_, execErr := td.Execute(context.Background(), json.RawMessage(`{"setting":"model"}`))
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(execErr, &unsupported) || !strings.Contains(unsupported.Error(), "A probe remedy") {
		t.Fatalf("an injected refusal answered %v; want the typed refusal carrying its remedy", execErr)
	}
}

// TestTheUnwiredSubagentRefusalCarriesTheHeadlineTheGrantReadsOn is the
// run-time half, and the one case that exists: a host that wires no subagent
// runner. The ultracode grant tells the model a refusal is final where the
// tool ANSWERS the headline
// (delegate.ultracodeOrchestrationInstruction); a refusal whose error omits it
// leaves that instruction keyed on words the model never receives, and the
// retry loop the grant exists to kill comes back.
func TestTheUnwiredSubagentRefusalCarriesTheHeadlineTheGrantReadsOn(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	td, err := reg.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	out, execErr := td.Execute(context.Background(), json.RawMessage(`{}`))
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(execErr, &unsupported) {
		t.Fatalf("an unwired `agent` returned (%q, %v); want the typed capability refusal", out, execErr)
	}
	refusal := toolcatalog.ClawUnwiredSubagentRefusal
	if !strings.Contains(unsupported.Error(), refusal.Remedy) {
		t.Errorf("the refusal does not carry the remedy %q: %s", refusal.Remedy, unsupported.Error())
	}
	if !strings.Contains(unsupported.Error(), toolcatalog.ClawRefusedToolHeadline) {
		t.Errorf("the refusal does not carry the headline %q the ultracode grant conditions on: %s",
			toolcatalog.ClawRefusedToolHeadline, unsupported.Error())
	}
}
