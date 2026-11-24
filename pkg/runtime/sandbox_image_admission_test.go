package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// stockSandboxRef is a reference to an image iterion publishes, built from the
// built-in repository so the test cannot drift from the family the admission
// judges.
const stockSandboxRef = builtInSandboxImageRepo + ":3.225.0"

// imageAdmissionRun drives the PRODUCTION path — resolveAndStartSandbox — on a
// driver whose Prepare refuses, so reaching the driver is distinguishable from
// being refused by the admission: errCanaryStop means "admitted, and the
// refusal is not in front of the pulls", any *RuntimeError means refused.
func imageAdmissionRun(t *testing.T, wf *ir.Workflow, image string, resolver effectiveBackendResolver) error {
	t.Helper()
	t.Setenv("ITERION_MODE", "local") // pin the factory to docker,podman,noop
	d := &specCapturingDriver{hostBinds: true}
	_, err := resolveAndStartSandbox(context.Background(), SandboxParams{
		Workflow:         wf,
		RunID:            "run-image-admission",
		WorkspacePath:    t.TempDir(),
		RepoRoot:         t.TempDir(),
		DefaultImage:     image,
		EffectiveBackend: resolver,
		Drivers: map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return d, nil },
		},
		Logger:    iterlog.Nop(),
		EmitEvent: func(store.EventType, map[string]any) error { return nil },
	})
	return err
}

// agentWorkflow is a sandboxed one-agent run whose node names backend.
func agentWorkflow(id, backend, workflowDefault string) *ir.Workflow {
	return &ir.Workflow{
		Name:           "image_admission",
		DefaultBackend: workflowDefault,
		Sandbox:        &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)},
		Nodes: map[string]ir.Node{
			id: &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: id},
				LLMFields: ir.LLMFields{Backend: backend},
			},
		},
	}
}

// TestTheStockImageRefusesTheBackendItCarriesNoCLIFor is the refusal of
// #1643, taken where the IMAGE is known: a run whose sandbox will start from
// an image iterion publishes, carrying a node that dispatches to kimi, is
// refused BEFORE the driver prepares anything — not left to die at
// `exec: kimi: not found` inside the container.
//
// Mutations seen red: drop the admitSandboxImageBackends call in
// resolveAndStartSandbox → errCanaryStop comes back instead of the refusal;
// swap backendcaps.UnservedByImageFamily for !Offers → the custom-image case below
// reddens; drop the scoped cells from registry.json → the sandbox scope reads
// Unknown and nothing is refused.
func TestTheStockImageRefusesTheBackendItCarriesNoCLIFor(t *testing.T) {
	err := imageAdmissionRun(t, agentWorkflow("plan", backendcaps.Kimi, ""), stockSandboxRef, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) {
		t.Fatalf("resolveAndStartSandbox = %v, want a typed refusal — a run whose image carries no kimi CLI must not reach the pulls", err)
	}
	if errors.Is(err, errCanaryStop) {
		t.Fatal("the driver prepared the sandbox before the refusal: the admission is in the wrong place")
	}
	if rerr.Code != ErrCodeCapabilityUnsupported {
		t.Errorf("refusal code = %q, want %q — the record an operator reads keys on it", rerr.Code, ErrCodeCapabilityUnsupported)
	}
	if rerr.NodeID != "plan" {
		t.Errorf("refusal names node %q, want %q", rerr.NodeID, "plan")
	}
	// The three things the decision requires it to NAME (ARB-82 B): the
	// image, the missing executable, and both ways out.
	for _, want := range []string{stockSandboxRef, backendcaps.Kimi, `"kimi"`} {
		if !strings.Contains(rerr.Message, want) {
			t.Errorf("refusal message %q does not name %q", rerr.Message, want)
		}
	}
	for _, want := range []string{"sandbox: none", "ITERION_SANDBOX_OVERRIDE", "image", "ITERION_SANDBOX_DEFAULT_IMAGE"} {
		if !strings.Contains(rerr.Hint, want) {
			t.Errorf("refusal hint %q does not offer %q", rerr.Hint, want)
		}
	}
	// The remedy names what the images DO serve, read from the registry: a
	// hand-written list here is the drift #1643 started from.
	for _, served := range backendcaps.ServedByImage(backendcaps.ImageFamilySandbox) {
		if !strings.Contains(rerr.Hint, served) {
			t.Errorf("refusal hint %q does not name %q, which the published images serve", rerr.Hint, served)
		}
	}
}

// TestACustomImageIsNeverRefusedForTheBackendItMayShip is the hatch, and it is
// the PREDICATE and not a flag: the refusal speaks only for images iterion
// publishes, because only their contents are measured here. An operator who
// bakes the CLI into their own image keeps their run.
func TestACustomImageIsNeverRefusedForTheBackendItMayShip(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/an-operator/iterion-sandbox-slim:with-kimi", // their namespace, their contents
		"registry.example.invalid:5000/team/sandbox:1",       // a registry port is not a tag
		builtInSandboxImageRepo + "-agents:3.225.0",          // a sibling repository nobody publishes
	} {
		err := imageAdmissionRun(t, agentWorkflow("plan", backendcaps.Kimi, ""), image, nil)
		if !errors.Is(err, errCanaryStop) {
			t.Errorf("image %q: resolveAndStartSandbox = %v, want the driver to be reached — refusing an image whose contents nobody measured is the artificial refusal #1643 excludes", image, err)
		}
	}
}

// TestTheSandboxAndRunnerFamiliesAreJudgedApart is ARB-82's measure 1, where
// it decides something: codex is on the runner images' PATH and outside the
// stock sandbox's, so the stock sandbox refuses it while the registry still
// reports it served on the runner side. One merged "partial" verdict could say
// neither.
func TestTheSandboxAndRunnerFamiliesAreJudgedApart(t *testing.T) {
	if got := backendcaps.Verdict(backendcaps.Codex, backendcaps.ImagePresence); got != backendcaps.Partial {
		t.Fatalf("codex image-presence = %q, want %q — this test's whole point is the family split", got, backendcaps.Partial)
	}
	if !backendcaps.UnservedByImageFamily(backendcaps.Codex, backendcaps.ImageFamilySandbox) {
		t.Fatal("codex reads as served in the SANDBOX family: the stock sandbox extends no PATH and symlinks only claude and pi")
	}
	if backendcaps.UnservedByImageFamily(backendcaps.Codex, backendcaps.ImageFamilyRunner) {
		t.Fatal("codex reads as unserved in the RUNNER family: the runner images carry the pinned bin dir on PATH")
	}
	err := imageAdmissionRun(t, agentWorkflow("review", backendcaps.Codex, ""), stockSandboxRef, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || !strings.Contains(rerr.Message, backendcaps.Codex) {
		t.Fatalf("a codex node on the stock sandbox image: resolveAndStartSandbox = %v, want a refusal naming codex", err)
	}
}

// TestTheImageAdmissionAdmitsEverythingItHasNotMeasured holds the refusal to
// what the registry says explicitly. Each of these reaching the driver is a
// separate guarantee, and a single predicate shrunk to the shape of one probe
// would lose the others.
func TestTheImageAdmissionAdmitsEverythingItHasNotMeasured(t *testing.T) {
	for _, tc := range []struct {
		name string
		wf   *ir.Workflow
	}{
		{"a backend the images serve", agentWorkflow("plan", backendcaps.ClaudeCode, "")},
		{"the implicit claw default", agentWorkflow("plan", "", "")},
		{"an explicit auto route", agentWorkflow("plan", "auto", "")},
		{"an env reference dispatch resolves", agentWorkflow("plan", "${ITERION_V121_BACKEND:-claude_code}", "")},
		{"a vars reference dispatch resolves", agentWorkflow("plan", "{{vars.backend}}", "")},
		{"a custom registration the registry never measured", agentWorkflow("plan", "my_own_cli", "")},
		{"a node kind whose backend the resolver invents", &ir.Workflow{
			Name:    "image_admission",
			Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)},
			Nodes:   map[string]ir.Node{"shell": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "shell"}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := imageAdmissionRun(t, tc.wf, stockSandboxRef, nil); !errors.Is(err, errCanaryStop) {
				t.Errorf("resolveAndStartSandbox = %v, want the driver to be reached", err)
			}
		})
	}
}

// TestTheWorkflowDefaultAndTheDispatchOverrideBothDecide: the refusal reads the
// route DISPATCH will take. A node with no backend of its own inherits the
// workflow's default, and the launch-time override (--backend, the resolver
// the executor implements) outranks both — a refusal that read the IR alone
// would miss the very route the operator chose at launch.
func TestTheWorkflowDefaultAndTheDispatchOverrideBothDecide(t *testing.T) {
	t.Run("the workflow default", func(t *testing.T) {
		err := imageAdmissionRun(t, agentWorkflow("plan", "", backendcaps.Grok), stockSandboxRef, nil)
		var rerr *RuntimeError
		if !errors.As(err, &rerr) || !strings.Contains(rerr.Message, backendcaps.Grok) {
			t.Fatalf("resolveAndStartSandbox = %v, want a refusal naming grok (the workflow's default_backend)", err)
		}
	})
	t.Run("the dispatch override outranks the IR", func(t *testing.T) {
		wf := agentWorkflow("plan", backendcaps.ClaudeCode, "")
		err := imageAdmissionRun(t, wf, stockSandboxRef, fixedBackendResolver(backendcaps.OpenCode))
		var rerr *RuntimeError
		if !errors.As(err, &rerr) || !strings.Contains(rerr.Message, backendcaps.OpenCode) {
			t.Fatalf("resolveAndStartSandbox = %v, want a refusal naming opencode — the node declares claude_code and the launch routed it elsewhere", err)
		}
	})
	// The engine NEVER hands back a nil resolver: an executor that cannot
	// report its routing gets the unansweredBackend stand-in, whose answer is
	// a sentence ("<unknown: … does not report the backend it routes to>").
	// Taken for a route, it would silence this refusal for every such
	// embedder — the IR reading would never run.
	t.Run("the stand-in for an executor that cannot say is not a route", func(t *testing.T) {
		err := imageAdmissionRun(t, agentWorkflow("plan", backendcaps.Kimi, ""), stockSandboxRef,
			unansweredBackend{executorType: "*runtime.stubExecutor"})
		var rerr *RuntimeError
		if !errors.As(err, &rerr) || !strings.Contains(rerr.Message, backendcaps.Kimi) {
			t.Fatalf("resolveAndStartSandbox = %v, want a refusal naming kimi — the executor said nothing, so the IR answers", err)
		}
	})
	// …while a resolver that DOES name a route is believed, even when the
	// registry has never heard of that backend: the run goes there, and the
	// node's own `backend:` is not where it lands.
	t.Run("a route the registry never measured silences the IR's", func(t *testing.T) {
		if err := imageAdmissionRun(t, agentWorkflow("plan", backendcaps.Kimi, ""), stockSandboxRef,
			fixedBackendResolver("my_own_cli")); !errors.Is(err, errCanaryStop) {
			t.Fatalf("resolveAndStartSandbox = %v, want the driver to be reached", err)
		}
	})
	t.Run("an undecided resolver leaves the IR to answer", func(t *testing.T) {
		err := imageAdmissionRun(t, agentWorkflow("plan", backendcaps.ClaudeCode, ""), stockSandboxRef, fixedBackendResolver(""))
		if !errors.Is(err, errCanaryStop) {
			t.Fatalf("resolveAndStartSandbox = %v, want the driver to be reached", err)
		}
	})
}

// TestAnUnsandboxedRunIsNeverRefusedForAnImageItNeverEnters: `sandbox: none`
// is one of the two exits the refusal itself offers, so taking it must work —
// and the cloud runner, which carries the pinned CLIs on PATH, is exactly the
// unsandboxed case the gap does not describe.
func TestAnUnsandboxedRunIsNeverRefusedForAnImageItNeverEnters(t *testing.T) {
	wf := agentWorkflow("plan", backendcaps.Kimi, "")
	wf.Sandbox = &ir.SandboxSpec{Mode: string(sandbox.ModeNone)}
	if err := imageAdmissionRun(t, wf, stockSandboxRef, nil); err != nil {
		t.Fatalf("resolveAndStartSandbox(sandbox: none) = %v, want nil — the run enters no image", err)
	}
}

// TestTheRefusalWordsItselfTheSameWayEveryStart: Workflow.Nodes is a map, so
// the refusal's message and the node it blames are a map walk away from
// changing on every start — and an operator comparing two runs would read two
// different stories about one gap. Five unserved nodes, twenty starts.
func TestTheRefusalWordsItselfTheSameWayEveryStart(t *testing.T) {
	wf := &ir.Workflow{
		Name:    "image_admission",
		Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)},
		Nodes:   map[string]ir.Node{},
	}
	for id, backend := range map[string]string{
		"zeta": backendcaps.Kimi, "alpha": backendcaps.Grok, "mu": backendcaps.OpenCode,
		"beta": backendcaps.Kimi, "omega": backendcaps.Grok,
	} {
		wf.Nodes[id] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: backend}}
	}
	first := ""
	for i := 0; i < 20; i++ {
		err := imageAdmissionRun(t, wf, stockSandboxRef, nil)
		var rerr *RuntimeError
		if !errors.As(err, &rerr) {
			t.Fatalf("start %d: resolveAndStartSandbox = %v, want a refusal", i, err)
		}
		if rerr.NodeID != "alpha" {
			t.Fatalf("start %d: the refusal blames node %q, want the first by id (%q)", i, rerr.NodeID, "alpha")
		}
		if first == "" {
			first = rerr.Message
			continue
		}
		if rerr.Message != first {
			t.Fatalf("start %d worded the same gap differently:\n%s\n%s", i, first, rerr.Message)
		}
	}
	// Grouped by backend, each with its own nodes: three sentences, not five.
	for _, want := range []string{
		`grok, whose CLI is "grok" (nodes alpha, omega)`,
		`kimi, whose CLI is "kimi" (nodes beta, zeta)`,
		`opencode, whose CLI is "opencode" (nodes mu)`,
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the refusal %q does not carry %q", first, want)
		}
	}
}

// TestADialIsReadTheWayTheRunReadsIt: a `backend:` written
// `${VAR:-kimi}` is the route its DEFAULT names only while the environment
// says nothing. With VAR set, the environment IS the route (#1389) — and a
// refusal that read the default there would kill a run that works. Both
// directions, because only the pair proves the reading is the run's and not
// the source's.
func TestADialIsReadTheWayTheRunReadsIt(t *testing.T) {
	const field = "${ITERION_V121_DIAL:-kimi}"
	t.Run("the default decides when the environment says nothing", func(t *testing.T) {
		t.Setenv("ITERION_V121_DIAL", "")
		os.Unsetenv("ITERION_V121_DIAL")
		err := imageAdmissionRun(t, agentWorkflow("plan", field, ""), stockSandboxRef, nil)
		var rerr *RuntimeError
		if !errors.As(err, &rerr) || !strings.Contains(rerr.Message, backendcaps.Kimi) {
			t.Fatalf("resolveAndStartSandbox = %v, want a refusal naming kimi — a dial's default is as knowable as a literal", err)
		}
	})
	t.Run("the environment outranks the default", func(t *testing.T) {
		t.Setenv("ITERION_V121_DIAL", backendcaps.ClaudeCode)
		if err := imageAdmissionRun(t, agentWorkflow("plan", field, ""), stockSandboxRef, nil); !errors.Is(err, errCanaryStop) {
			t.Fatalf("resolveAndStartSandbox = %v, want the driver to be reached — the run routes to claude_code, which the image serves", err)
		}
	})
}

// TestTheStockSandboxFamilyIsTheOneTheTreeBuilds holds the repository list the
// predicate judges to the images the tree actually publishes: a fifth
// `sandbox/<variant>/Dockerfile` must either be in the family or be a
// deliberate omission, never an oversight that silently stops refusing.
func TestTheStockSandboxFamilyIsTheOneTheTreeBuilds(t *testing.T) {
	if !strings.HasSuffix(builtInSandboxImageRepo, "-slim") {
		t.Fatalf("builtInSandboxImageRepo = %q: the family is derived by swapping the slim variant, so this name decides what isStockSandboxImage can recognise", builtInSandboxImageRepo)
	}
	entries, err := os.ReadDir("../../sandbox")
	if err != nil {
		t.Fatalf("the sandbox image manifests are unreadable, so this guard certifies nothing: %v", err)
	}
	var variants []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "finalize" {
			// finalize is the shared last layer of every variant, not a
			// variant of its own: no image is published under its name.
			continue
		}
		if _, err := os.Stat(filepath.Join("../../sandbox", e.Name(), "Dockerfile")); err != nil {
			continue
		}
		variants = append(variants, e.Name())
	}
	if len(variants) == 0 {
		t.Fatal("no sandbox variant found: this guard is reading the wrong directory")
	}
	prefix := strings.TrimSuffix(builtInSandboxImageRepo, "slim")
	for _, variant := range variants {
		if !isStockSandboxImage(prefix + variant + ":edge") {
			t.Errorf("sandbox/%s/Dockerfile is published as %s%s, which the admission does not recognise as a stock image — a run on it would never be refused", variant, prefix, variant)
		}
	}
	if !isStockSandboxImage(builtInSandboxImageRepo + "@sha256:" + strings.Repeat("ab", 32)) {
		t.Error("a digest-pinned stock image is not recognised — production deployments pin by digest")
	}
	if isStockSandboxImage("") {
		t.Error("an empty image reference reads as stock: a run with no image resolved would be refused on nothing")
	}
}
