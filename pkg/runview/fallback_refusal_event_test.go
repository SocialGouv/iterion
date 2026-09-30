package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// buildWithRunFallback runs BuildExecutor over a one-agent workflow and
// returns the run's timeline.
func buildWithRunFallback(t *testing.T, tools []string) []*store.Event {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	agent := &ir.AgentNode{}
	agent.ID = "work"
	agent.Backend = "claude_code"
	agent.Tools = tools

	spec := ExecutorSpec{
		Ctx:      context.Background(),
		Store:    st,
		RunID:    "run-fallback-refusal",
		Workflow: &ir.Workflow{Name: "canary", Nodes: map[string]ir.Node{"work": agent}},
		// The operator's launch-time ask: survive a forfait wall by
		// crossing to another backend.
		RunFallback: []ir.Fallback{{Backend: "claw", Model: "openai/gpt-5.5"}},
	}
	if _, err := BuildExecutor(spec); err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	evts, err := st.LoadEvents(context.Background(), spec.RunID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return evts
}

func refusalEvents(evts []*store.Event) []*store.Event {
	var out []*store.Event
	for _, e := range evts {
		if e.Type == store.EventRunFallbackRefused {
			out = append(out, e)
		}
	}
	return out
}

// Un refus que le décideur ne peut pas lire est un fallback silencieux :
// l'opérateur a demandé la route, l'écran la décline pour une raison qu'il
// énonce précisément, et cette raison doit atteindre la timeline du run — pas
// seulement le log du runner, que l'opérateur n'a pas.
func TestBuildExecutor_RunFallbackRefusalReachesTheTimeline(t *testing.T) {
	// Aucun `tools:` déclaré : la traversée vers un backend CLI donnerait
	// silencieusement l'outillage complet, et l'écran refuse.
	refusals := refusalEvents(buildWithRunFallback(t, nil))
	if len(refusals) != 1 {
		t.Fatalf("attendu 1 événement %s, obtenu %d", store.EventRunFallbackRefused, len(refusals))
	}
	reason, _ := refusals[0].Data["reason"].(string)
	if !strings.Contains(reason, "work") {
		t.Errorf("la raison doit nommer le nœud refusé, obtenu %q", reason)
	}
	if !strings.Contains(reason, "tools") {
		t.Errorf("la raison doit porter le diagnostic actionnable de l'écran, obtenu %q", reason)
	}
}

// L'autre face, sans laquelle le banc ne prouverait rien : une route PRISE
// ne doit rien écrire. Un témoin qui parle dans les deux cas ne témoigne pas.
func TestBuildExecutor_TakenRunFallbackStaysSilent(t *testing.T) {
	if refusals := refusalEvents(buildWithRunFallback(t, []string{"read_file"})); len(refusals) != 0 {
		t.Fatalf("une route prise ne doit émettre aucun refus, obtenu %d : %+v",
			len(refusals), refusals)
	}
}

// buildWithRunFallbackVars runs BuildExecutor over a one-agent workflow
// whose backend is a `{{vars.b}}` reference, with the launch's vars, and
// returns the run's timeline.
func buildWithRunFallbackVars(t *testing.T, def string, vars map[string]string) []*store.Event {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	agent := &ir.AgentNode{}
	agent.ID = "work"
	agent.Backend = "{{vars.b}}"

	spec := ExecutorSpec{
		Ctx:   context.Background(),
		Store: st,
		RunID: "run-fallback-vars",
		Workflow: &ir.Workflow{
			Name:  "canary",
			Nodes: map[string]ir.Node{"work": agent},
			Vars:  map[string]*ir.Var{"b": {Name: "b", Type: ir.VarString, HasDefault: true, Default: def}},
		},
		Vars: vars,
		// A CLI route: a crossing iff the run's reading of the node is claw.
		RunFallback: []ir.Fallback{{Backend: "claude_code", Model: "claude-opus-5"}},
	}
	if _, err := BuildExecutor(spec); err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	evts, err := st.LoadEvents(context.Background(), spec.RunID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return evts
}

// L'écran de lancement lit un `backend: "{{vars.b}}"` avec les vars DU
// LANCEMENT : la valeur par défaut déclarée fait refuser la traversée vers
// un backend CLI sur un nœud sans `tools:`, et l'override `--var` de
// l'opérateur — pas la valeur du source — est la lecture qui l'autorise.
func TestBuildExecutor_RunFallbackScreensAVarsBackendWithTheLaunchVars(t *testing.T) {
	refusals := refusalEvents(buildWithRunFallbackVars(t, "claw", nil))
	if len(refusals) != 1 {
		t.Fatalf("attendu 1 refus (nœud claw sans tools:, traversée CLI), obtenu %d", len(refusals))
	}
	// Le même lancement avec --var b=claude_code : le nœud EST claude_code
	// pour ce run, la route ne traverse rien — aucun refus.
	if refusals := refusalEvents(buildWithRunFallbackVars(t, "claw", map[string]string{"b": "claude_code"})); len(refusals) != 0 {
		t.Fatalf("l'override du lancement doit décider la lecture du nœud, obtenu %d refus : %+v",
			len(refusals), refusals)
	}
}
