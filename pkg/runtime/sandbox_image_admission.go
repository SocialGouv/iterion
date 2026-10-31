package runtime

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/imageclass"
)

// stockSandboxVariants are the sandbox images iterion publishes, one per
// `sandbox/<variant>/Dockerfile` (`finalize` is the shared last layer of all
// of them, not a variant of its own) — [imageclass.Classes], the vocabulary
// a `sandbox: { image: <class> }` declaration names them by (#2632). The
// repositories are derived from [builtInSandboxImageRepo] — which IS the
// slim one — so the family and the default `sandbox: auto` resolves to
// cannot separate; TestTheStockSandboxFamilyIsTheOneTheTreeBuilds holds the
// list to the tree.
//
// The `-base` tags the image pipeline also publishes are deliberately NOT
// here: the image-presence derivation measures the FINALIZED chain, and a base
// layer's contents are a fact nobody measured. A run on one keeps the old
// `exec: not found`, which is the honest answer for an unmeasured image.
var stockSandboxVariants = imageclass.Classes()

// isStockSandboxImage reports whether ref names a sandbox image ITERION
// publishes: the repository, tag and digest aside, is one of
// `<builtInSandboxImageRepo's namespace>/iterion-sandbox-<variant>`.
//
// Deliberately the full repository and not a name suffix. An image an operator
// builds — even from these very manifests, under their own namespace, with the
// CLI they need baked in — is THEIRS, and what it ships is measured by nobody
// here: reading it as stock would refuse a run that works. The admission below
// therefore under-refuses by construction, which is the direction philosophy
// #1 asks for: the limit is the image's contents, and only the images iterion
// itself publishes are a contents iterion can certify (#1643).
func isStockSandboxImage(ref string) bool {
	repo := imageRepository(ref)
	if repo == "" {
		return false
	}
	prefix := strings.TrimSuffix(builtInSandboxImageRepo, "slim")
	if prefix == builtInSandboxImageRepo {
		// The built-in stopped being the slim variant: say nothing rather
		// than judge a family this function can no longer name.
		return false
	}
	for _, variant := range stockSandboxVariants {
		if repo == prefix+variant {
			return true
		}
	}
	return false
}

// imageRepository returns the repository part of an image reference: the tag
// (`:<tag>`) and the digest (`@sha256:…`) removed. The tag separator is the
// last colon AFTER the last slash, so a registry port (`registry:5000/x`)
// keeps its own colon.
func imageRepository(ref string) string {
	repo := strings.TrimSpace(ref)
	if at := strings.Index(repo, "@"); at >= 0 {
		repo = repo[:at]
	}
	if colon := strings.LastIndex(repo, ":"); colon > strings.LastIndex(repo, "/") {
		repo = repo[:colon]
	}
	return repo
}

// unservedByImage is one node whose backend has no CLI in the image the run's
// sandbox will start from.
type unservedByImage struct {
	node    string
	backend string
	binary  string
}

// admitSandboxImageBackends refuses a run whose sandbox will start from an
// image iterion publishes while a node it will dispatch runs on a backend that
// image provably carries no CLI for — kimi, grok and opencode today, and codex
// on the sandbox side, where the pinned tree is present but off PATH
// (`image-presence`, derived from the image manifests: #1643).
//
// Called where the IMAGE is known and the sandbox is about to start, which is
// the only place the refusal can be honest. Earlier surfaces — the launch API,
// the compiler — see a backend NAME and no image, so they warn (C321,
// `imagePresenceWarnings`) and admit: an operator whose custom image bakes
// opencode must not be refused a name their image serves, and `sandbox: auto`
// on a host with no driver never enters an image at all.
//
// What it does NOT refuse, on purpose:
//
//   - a `fallbacks:` route on such a backend. The route is a survival path the
//     run may never take, and killing the run for it would remove the chain
//     the primary's exhaustion needs; the launch screen already drops the
//     routes it can judge (ApplyRunFallback).
//   - an undecided name: an empty backend with no workflow default, an
//     `auto`, a `{{vars.…}}`, a value the IR stores unexpanded. The dispatch
//     resolver answers for those, and a refusal on a guess is worse than the
//     `exec: not found` it replaces.
//   - a backend the registry does not know. A custom registration's image is
//     the operator's too — [backendcaps.UnservedByImageFamily] reports a
//     measured absence, never the negation of a silence.
func admitSandboxImageBackends(wf *ir.Workflow, spec *sandbox.Spec, resolver effectiveBackendResolver) error {
	if wf == nil || spec == nil || !spec.Mode.IsActive() {
		return nil
	}
	// A built image is the operator's recipe, whatever it is FROM: its
	// contents are measured by nobody here.
	if spec.Build != nil || !isStockSandboxImage(spec.Image) {
		return nil
	}
	var unserved []unservedByImage
	for _, n := range wf.Nodes {
		backend := dispatchedBackendName(n, wf, resolver)
		if backend == "" || !backendcaps.UnservedByImageFamily(backend, backendcaps.ImageFamilySandbox) {
			continue
		}
		unserved = append(unserved, unservedByImage{
			node:    n.NodeID(),
			backend: backend,
			binary:  backendcaps.ImageCLI(backend),
		})
	}
	if len(unserved) == 0 {
		return nil
	}
	// Workflow.Nodes is a map: sorted by node id, or the same run's refusal
	// blames a different node and words its list differently on every start.
	slices.SortFunc(unserved, func(a, b unservedByImage) int {
		return strings.Compare(a.node, b.node)
	})
	return &RuntimeError{
		Code:    ErrCodeCapabilityUnsupported,
		NodeID:  unserved[0].node,
		Message: fmt.Sprintf("sandbox: %s", describeUnservedByImage(spec.Image, unserved)),
		Hint: fmt.Sprintf(
			"two ways out: run those nodes unsandboxed (`sandbox: none` on the workflow, --sandbox none, or %s=none), or start the sandbox from an image that ships the CLI (`sandbox: { image: … }` or `build:` in the workflow, --sandbox-default-image, %s) — the images iterion publishes serve %s",
			"ITERION_SANDBOX_OVERRIDE", EnvSandboxDefaultImage,
			strings.Join(backendcaps.ServedByImage(backendcaps.ImageFamilySandbox), ", ")),
	}
}

// describeUnservedByImage names the image, every backend it cannot serve with
// the executable each one invokes, and the nodes that would have run there —
// grouped by backend, because the operator's gesture is per backend and a
// per-node repetition of the same sentence buries it.
func describeUnservedByImage(image string, unserved []unservedByImage) string {
	order := make([]string, 0, len(unserved))
	nodes := map[string][]string{}
	binaries := map[string]string{}
	for _, u := range unserved {
		if _, seen := nodes[u.backend]; !seen {
			order = append(order, u.backend)
		}
		nodes[u.backend] = append(nodes[u.backend], u.node)
		binaries[u.backend] = u.binary
	}
	parts := make([]string, 0, len(order))
	for _, backend := range order {
		binary := binaries[backend]
		if binary == "" {
			// The cell names no executable: say what is known rather than
			// print an empty pair of quotes.
			parts = append(parts, fmt.Sprintf("%s (nodes %s)", backend, strings.Join(nodes[backend], ", ")))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s, whose CLI is %q (nodes %s)", backend, binary, strings.Join(nodes[backend], ", ")))
	}
	return fmt.Sprintf("image %s puts no executable on PATH for %s", image, strings.Join(parts, "; "))
}

// dispatchedBackendName is the backend a node will run on, as far as a
// pre-run reading can be SURE of it: the dispatch resolver's answer when it
// has one (it carries the launch-time `--backend` override), else the run's
// own reading of the node's `backend:` and the workflow's `default_backend:`
// ([ir.RunNodeBackendName] — the rule dispatch itself applies, with the
// process environment as the route).
//
// "" is everything undecided — a name left to the host's default and its
// credential probe, an `auto` no default answers, a `{{vars.…}}` the dispatch
// resolves with the run's vars — so a caller that REFUSES on this answer
// never refuses a guess. That is the opposite tie-break from [backendIsClaw],
// which reads an empty backend as claw and errs toward a harmless bind mount.
func dispatchedBackendName(n ir.Node, wf *ir.Workflow, resolver effectiveBackendResolver) string {
	switch n.(type) {
	case *ir.AgentNode, *ir.JudgeNode, *ir.RouterNode:
	default:
		// Every other kind falls through the resolver's chain to its
		// last-resort arm, which names a backend the node never declared.
		return ""
	}
	// The resolver answers for dispatch, launch-time override included — but
	// the engine never hands back nil: an executor that cannot report where it
	// routes gets the [unansweredBackend] stand-in, whose answer is a sentence
	// and not a backend. Recognised by TYPE, as the workspace-safety admission
	// does, because taking that sentence for a route would short-circuit the
	// IR reading and silence this refusal for every such embedder. An empty
	// answer is the same silence, documented on the seam.
	if _, unanswered := resolver.(unansweredBackend); resolver != nil && !unanswered {
		if name := ir.SourceBackendName(resolver.EffectiveBackendName(n)); name != "" {
			return strings.ToLower(strings.TrimSpace(name))
		}
	}
	declared := ""
	switch nn := n.(type) {
	case *ir.AgentNode:
		declared = nn.Backend
	case *ir.JudgeNode:
		declared = nn.Backend
	case *ir.RouterNode:
		declared = nn.Backend
	}
	return strings.ToLower(strings.TrimSpace(ir.RunNodeBackendName(declared, wf.DefaultBackend)))
}
