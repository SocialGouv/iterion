package bots

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// deliveryProbeMarker is the one line only the delivery permission probe
// carries: the digest of the token actually mounted in the run, sent to the
// scoped preflight endpoint. Reading carriers off the COMPILED command yields
// the node name for free, so a bot that spells its probe node differently is
// still found.
const deliveryProbeMarker = "'credential_sha256': hashlib.sha256(token.encode()).hexdigest()"

type deliveryProbeCarrier struct {
	rel     string
	node    string
	command string
	program string
}

// discoverDeliveryProbeCarriers compiles every bundle and returns each tool
// node that asks the delivery preflight whether the mounted token may push a
// workflow file.
func discoverDeliveryProbeCarriers(t *testing.T) []deliveryProbeCarrier {
	t.Helper()
	mains, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	var found []deliveryProbeCarrier
	declares, matched := map[string]bool{}, map[string]bool{}
	for _, rel := range mains {
		pr := parseBotUnit(rel)
		if pr.File == nil {
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue
		}
		// A SECOND, independent signal that this bundle gates its delivery on
		// the probe: it declares the probe's own result schema. Independent of
		// how the command spells the payload, which is how the discriminator
		// above can go stale without anyone noticing.
		if cr.Workflow.Schemas["delivery_probe_state"] != nil {
			declares[rel] = true
		}
		for node, raw := range cr.Workflow.Nodes {
			tn, ok := raw.(*ir.ToolNode)
			if !ok || !strings.Contains(tn.Command, deliveryProbeMarker) {
				continue
			}
			found = append(found, deliveryProbeCarrier{
				rel: rel, node: node, command: tn.Command,
				program: pythonProgramOf(t, rel, node, tn.Command),
			})
			matched[rel] = true
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].rel != found[j].rel {
			return found[i].rel < found[j].rel
		}
		return found[i].node < found[j].node
	})
	// The cross-check runs first because it names the culprit: a bundle that
	// declares the schema and matched no node either spells the payload
	// differently or ships a probe this guard never reads.
	var unmatched []string
	for rel := range declares {
		if !matched[rel] {
			unmatched = append(unmatched, rel)
		}
	}
	if len(unmatched) > 0 {
		sort.Strings(unmatched)
		t.Fatalf("%v declare a delivery_probe_state schema and no node of theirs matched %q — either the discriminator is stale or these carriers ship an unguarded copy",
			unmatched, deliveryProbeMarker)
	}
	// A floor, because the other failure mode of a discriminator is to match
	// nothing and leave a vacuous guard looking green. Two carriers is what
	// makes "the copies agree" a statement at all.
	if len(found) < 2 {
		t.Fatalf("discovered %d delivery-probe carriers, want >= 2 (branch-improve-loop and dep-update-guard) — the discriminator %q is stale, or a bot that pushes a workflow alignment carries no probe",
			len(found), deliveryProbeMarker)
	}
	return found
}

// pythonProgramOf returns the python source a probe runs: everything the
// carrier passes to `python3 -I -c`, with the per-bot environment prefix left
// out. The prefix is where the bots legitimately differ — each binds the base
// its own push is measured against — and the program is where they must not.
func pythonProgramOf(t *testing.T, rel, node, command string) string {
	t.Helper()
	const open = "python3 -I -c \""
	i := strings.Index(command, open)
	if i < 0 {
		t.Fatalf("%s/%s: the probe command does not run `python3 -I -c \"…\"`; the isolated interpreter is what keeps a planted module out of a credential check", rel, node)
	}
	program := command[i+len(open):]
	if !strings.HasSuffix(program, "\"") {
		t.Fatalf("%s/%s: the probe program is not closed by a quote — the extraction is wrong, not the bot", rel, node)
	}
	return strings.TrimSuffix(program, "\"")
}

// TestDeliveryProbeCopiesAreIdentical is the anti-drift guard this probe needs
// because it CANNOT be shared: pkg/dsl/unit refuses an import outside the
// bot's own lib/ (E045), so every bot that must prove its delivery permission
// carries a copy of the same ~90 lines — the token read, the digest, the
// no-redirect opener, the typed refusals.
//
// That is the shape this repository has already paid for twice: a defect
// reported against one bot and living in ten. A copy with no guard drifts
// silently, and the halves that would drift first are the ones a reader
// trusts most — the refusal wording a hold shows an operator, and the branch
// that decides a token is good enough.
//
// Deliberately NOT asserted: the environment prefix. branch-improve-loop
// measures the whole branch it is about to bank (BASE={{vars.base_ref}});
// dep-update-guard measures what `commit` is about to make out of the working
// tree, the dependency bot's bump being already on the remote (BASE=HEAD).
// Each binding is pinned, with its reason, by that bot's own tests.
func TestDeliveryProbeCopiesAreIdentical(t *testing.T) {
	carriers := discoverDeliveryProbeCarriers(t)
	reference := carriers[0]
	for _, c := range carriers[1:] {
		if c.program == reference.program {
			continue
		}
		t.Errorf("%s/%s and %s/%s run different delivery probes — copy the whole program across, or the two bots refuse different deliveries under one code.\nFirst divergence:\n  %s\n  %s",
			reference.rel, reference.node, c.rel, c.node,
			firstDifferingLine(reference.program, c.program), firstDifferingLine(c.program, reference.program))
	}
}

// TestDeliveryProbeCarriersRefuseAnUnprovableDelivery pins the contract every
// copy owes, from the compiled program rather than from prose: an unknown
// permission is a TYPED refusal, no refusal ever echoes the credential, and
// the interpreter is isolated.
func TestDeliveryProbeCarriersRefuseAnUnprovableDelivery(t *testing.T) {
	for _, c := range discoverDeliveryProbeCarriers(t) {
		t.Run(c.rel+"/"+c.node, func(t *testing.T) {
			for _, want := range []struct{ frag, why string }{
				{"FORGE_PERMISSION_DENIED", "a refusal with no code reads like any other failure on the run"},
				{"FORGE_PREFLIGHT_UNAVAILABLE", "a service that did not answer, typed as the credential's refusal, makes an outage of ours read as a grant an operator must perform — and publishes it on a third party's pull request"},
				{"no scoped delivery preflight service is available", "a probe with no proof service must refuse, not assume"},
				{"class NoRedirect", "a redirect would carry the token digest to a host the grant never named"},
				{"reissue the token with the workflow and repo scopes", "a classic token short of the `workflow` scope is a third cause of this one refusal, and the operator behind it has no App and no grant to approve: the cure has to travel with the reason, since a carrier's own comment may offer none"},
			} {
				if !strings.Contains(c.program, want.frag) {
					t.Errorf("the probe no longer carries %q — %s", want.frag, want.why)
				}
			}
			// THE INVERSION, countable: `hold(` is the only path to a
			// blocking verdict on a pull request, and the program names it
			// three times — its definition and the two answered questions
			// (the preflight's verdict, the forge API's scope list). A
			// fourth is a new path to a hold, and the shape of every defect
			// this probe has had: one more failure mode enumerated into the
			// refusal rather than out of it.
			if n := strings.Count(c.program, "hold("); n != 3 {
				t.Errorf("the probe names hold( %d times, want 3 (the definition plus one answered denial per question asked) — every other outcome belongs on the unavailable path", n)
			}
			// The same invariant under the OTHER spelling: `hold` is a name,
			// `emit(False, True, ...)` is the blocking verdict itself, and a
			// caller that builds one directly answers to no count above.
			if n := strings.Count(c.program, "emit(False, True"); n != 1 {
				t.Errorf("the probe constructs the blocking verdict %d times, want 1 (inside hold) — a second constructor is a path to a red check on a third party's pull request that the count above cannot see", n)
			}
			// The token is read to be hashed and to be sent as a bearer. It
			// must never reach a reason string: the reason is published in a
			// hold comment on a public pull request.
			for _, forbidden := range []string{"emit(False, token", "reason': token", "+ token)"} {
				if strings.Contains(c.program, forbidden) {
					t.Errorf("the probe builds a message out of the credential (%q) — the reason is published on the pull request", forbidden)
				}
			}
		})
	}
}

// firstDifferingLine names where two copies parted, so a failure is
// actionable without a diff tool.
func firstDifferingLine(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range al {
		if i >= len(bl) {
			return "+" + al[i]
		}
		if al[i] != bl[i] {
			return "line " + strconv.Itoa(i+1) + ": " + al[i]
		}
	}
	if len(bl) > len(al) {
		return "(shorter by " + strconv.Itoa(len(bl)-len(al)) + " lines)"
	}
	return "(identical)"
}
