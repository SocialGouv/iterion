package bundle

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
)

// Each label key owes the release floor its pin names, through the one
// floors table: `validate` C252, the push admission and the scaffold all
// read it, so an author is told "this bundle needs iterion >= X" instead of
// a build below X refusing the whole manifest at its strict decode. The key
// is read from the manifest's own bytes, wherever an author can spell it:
// on any element of `invocations`, in block or flow style, with or without
// a value, and on the `forge:` block.
//
// floortest:exempt ForgeLabelsSince — the floor declared in the rows below
// is the one each case is AT, read from its own pin; the release BELOW it
// comes from floortest.Below, never from a sibling pin.
func TestTheForgeLabelKeysOweTheirReleaseFloor(t *testing.T) {
	const (
		invocationKey = "`invocations[].forge.labels`"
		blockKey      = "`forge.labels`"
	)
	manifests := map[string]struct {
		body    string
		reasons []string
	}{
		"labels on an invocation": {
			"name: probe\ninvocations:\n  - kind: forge\n    forge:\n      event: issue_labeled\n      labels: [nestor]\n",
			[]string{invocationKey},
		},
		"labels on an invocation, flow style": {
			"name: probe\ninvocations:\n  - kind: forge\n    forge: { event: issue_labeled, labels: [nestor] }\n",
			[]string{invocationKey},
		},
		"labels on a later invocation": {
			"name: probe\ninvocations:\n  - kind: board\n  - kind: forge\n    forge: { event: pull_request }\n  - kind: forge\n    forge: { event: issue_labeled, labels: [nestor] }\n",
			[]string{invocationKey},
		},
		"labels declared with no value on an invocation": {
			"name: probe\ninvocations:\n  - kind: forge\n    forge:\n      event: issue_labeled\n      labels:\n",
			[]string{invocationKey},
		},
		"labels on the forge block": {
			"name: probe\nforge:\n  events: [issue_labeled]\n  labels: [implement]\n",
			[]string{blockKey},
		},
		"labels declared with no value on the forge block": {
			"name: probe\nforge:\n  events: [issue_labeled]\n  labels:\n",
			[]string{blockKey},
		},
		"both keys": {
			"name: probe\nforge:\n  events: [issue_labeled]\n  labels: [implement]\ninvocations:\n  - kind: forge\n    forge: { event: issue_labeled, labels: [nestor] }\n",
			[]string{invocationKey, blockKey},
		},
	}
	floors := map[string]struct {
		requires string
		wantOK   bool
	}{
		"no floor":          {"", false},
		"a floor below":     {"requires:\n  iterion: \">= " + floortest.Below(t, ForgeLabelsSince) + "\"\n", false},
		"the floor itself":  {"requires:\n  iterion: \">= " + ForgeLabelsSince + "\"\n", true},
		"the floor, no >=":  {"requires:\n  iterion: \"" + ForgeLabelsSince + "\"\n", true},
		"a floor far below": {"requires:\n  iterion: \">= 0.0.1\"\n", false},
	}
	for manifestName, manifest := range manifests {
		for floorName, floor := range floors {
			t.Run(manifestName+"/"+floorName, func(t *testing.T) {
				body := manifest.body + floor.requires
				m, err := DecodeManifest([]byte(body), "probe")
				if err != nil {
					t.Fatalf("decode: %v\nmanifest:\n%s", err, body)
				}
				// The product predicate, never a floor the test holds
				// alone: what `validate` (C252) and the push admission read.
				pf := CheckSyntaxFloor(m, SyntaxRequirements{})
				if pf.OK != floor.wantOK {
					t.Fatalf("floor OK = %v, want %v: %+v\nmanifest:\n%s", pf.OK, floor.wantOK, pf, body)
				}
				if floor.wantOK {
					return
				}
				if pf.Need != ForgeLabelsSince {
					t.Fatalf("the verdict asks for %q, not the release that reads the label keys (%s): %+v", pf.Need, ForgeLabelsSince, pf)
				}
				for _, reason := range manifest.reasons {
					if !strings.Contains(pf.Uses, reason) {
						t.Errorf("the verdict does not name %s among what asks for the floor: %q", reason, pf.Uses)
					}
				}
				if len(manifest.reasons) == 1 && pf.Reason != manifest.reasons[0] {
					t.Errorf("the verdict's reason is %q, want the key the manifest spells, %s", pf.Reason, manifest.reasons[0])
				}
				if !strings.Contains(pf.Consequence, "refuses the whole document") {
					t.Errorf("the verdict does not say what a build below the floor does: %q", pf.Consequence)
				}
			})
		}
	}

	// A manifest that spells neither key owes nothing for them, forge block
	// and forge invocation included: the floor is the key's, not the block's.
	for name, body := range map[string]string{
		"a forge block and a forge invocation without labels": "name: probe\nforge:\n  events: [issue_labeled, pull_request]\ninvocations:\n  - kind: forge\n    forge: { event: issue_labeled, actions: [labeled] }\n  - kind: board\n    board: { all_labels: [ready] }\n",
		"no forge at all": "name: probe\n",
	} {
		t.Run(name, func(t *testing.T) {
			m, err := DecodeManifest([]byte(body), "probe")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if pf := CheckSyntaxFloor(m, SyntaxRequirements{}); !pf.OK || pf.Reason != "" {
				t.Fatalf("a manifest that declares no label key is asked for a floor: %+v", pf)
			}
		})
	}
}

// The key reader reaches an invocation's keys through the `invocations`
// sequence, under the sequence's own name and without an index, to the same
// depth as a mapping's; every other sequence stays a value whose elements
// are not key paths.
func TestDeclaredManifestKeysReadsTheInvocationsElements(t *testing.T) {
	body := "" +
		"name: probe\n" +
		"exports:\n  workflows:\n    - id: shared\n      path: shared.bot\n" +
		"invocations:\n" +
		"  - kind: command\n    command: { name: probe }\n" +
		"  - kind: forge\n    forge:\n      event: issue_labeled\n      labels: [nestor]\n" +
		"  - kind: board\n    board:\n      all_labels: [ready]\n"
	keys, err := DeclaredManifestKeys([]byte(body))
	if err != nil {
		t.Fatalf("reading the keys: %v", err)
	}
	for _, want := range []string{"invocations", "invocations.kind", "invocations.command", "invocations.command.name", "invocations.forge", "invocations.forge.event", keyInvocationForgeLabels, "invocations.board.all_labels"} {
		if !keys[want] {
			t.Errorf("key %q is not read as declared: %s", want, declaredKeyList(keys))
		}
	}
	for _, unwanted := range []string{"invocations.0", "invocations.1.forge.labels", "invocations[1].forge.labels", keyForgeLabels, "exports.workflows.id", "exports.workflows.path"} {
		if keys[unwanted] {
			t.Errorf("key %q is read as declared, and is not: %s", unwanted, declaredKeyList(keys))
		}
	}
	// The decoder records the same keys on the manifest it hands out, which
	// is what the floors table reads.
	m, err := DecodeManifest([]byte(body), "probe")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !m.DeclaredKeys.Declares(keyInvocationForgeLabels) || m.DeclaredKeys.Declares(keyForgeLabels) {
		t.Fatalf("the decoded manifest does not carry the invocation's label key alone: %s", declaredKeyList(m.DeclaredKeys))
	}
}
