package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

func TestContributionsFromWire_NilStaysNil(t *testing.T) {
	if got := contributionsFromWire(nil); got != nil {
		t.Errorf("nil wire payload must stay nil (local resolution), got %+v", got)
	}
}

// An EMPTY (but present) payload must convert to a non-nil value: the runner
// passes it to the engine to suppress the dead local lookup on a pod whose
// iterion home is empty. Converting it to nil would silently re-enable that.
func TestContributionsFromWire_EmptyStaysNonNil(t *testing.T) {
	got := contributionsFromWire(&queue.Contributions{})
	if got == nil {
		t.Fatal("an empty-but-present payload must convert to non-nil (authoritative)")
	}
	if !got.IsEmpty() {
		t.Errorf("expected empty payload, got %+v", got)
	}
}

func TestContributionsFromWire_CarriesBothKinds(t *testing.T) {
	got := contributionsFromWire(&queue.Contributions{
		Plugin: []queue.ContributionFile{
			{Kind: "skills", Name: "deploy-target.md", Content: []byte("playbook")},
		},
		Library: []queue.LibrarySkillFile{
			{Name: "changelog-writer", Description: "writes changelogs", Content: []byte("body")},
		},
	})
	if len(got.Plugin) != 1 || got.Plugin[0].Kind != "skills" ||
		got.Plugin[0].Name != "deploy-target.md" || string(got.Plugin[0].Content) != "playbook" {
		t.Errorf("plugin file not carried faithfully: %+v", got.Plugin)
	}
	if len(got.Library) != 1 || got.Library[0].Name != "changelog-writer" ||
		got.Library[0].Description != "writes changelogs" || string(got.Library[0].Content) != "body" {
		t.Errorf("library skill not carried faithfully: %+v", got.Library)
	}
}

// The payload has to survive the actual JSON envelope, since that is how it
// reaches the pod.
func TestContributions_SurvivesQueueJSONRoundtrip(t *testing.T) {
	in := &queue.RunMessage{
		V:     queue.SchemaVersion,
		RunID: "r1",
		Contributions: &queue.Contributions{
			Plugin:  []queue.ContributionFile{{Kind: "skills", Name: "deploy-target.md", Content: []byte("playbook")}},
			Library: []queue.LibrarySkillFile{{Name: "cw", Description: "d", Content: []byte("b")}},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out queue.RunMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Contributions == nil {
		t.Fatal("contributions lost in JSON roundtrip")
	}
	conv := contributionsFromWire(out.Contributions)
	if len(conv.Plugin) != 1 || string(conv.Plugin[0].Content) != "playbook" {
		t.Errorf("plugin file lost: %+v", conv.Plugin)
	}
	if len(conv.Library) != 1 || string(conv.Library[0].Content) != "b" {
		t.Errorf("library skill lost: %+v", conv.Library)
	}
}

// A message published WITHOUT contributions must leave the field absent, so an
// older consumer sees exactly what it saw before.
func TestContributions_OmittedWhenNil(t *testing.T) {
	raw, err := json.Marshal(&queue.RunMessage{V: queue.SchemaVersion, RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if _, present := generic["contributions"]; present {
		t.Error("contributions must be omitted from the wire when nil")
	}
}

// #1500 R6 medium, runner side: a dispatch WITHOUT the contributions payload
// is an anomaly (the publisher ships the field on every launch and every
// resume, possibly empty), so the runner must NOT translate it into a plain
// local-resolution engine. It flags the declaration unresolved — the engine
// mirrors nothing for the ambient tier and skips the orphan pruner instead of
// deleting the launch pass's files.
//
// The flag's EFFECT is pinned at the runtime end
// (TestMirrorPluginContributions_NilPayloadOnRunnerFlagsIncomplete); this
// pins the runner end: one option, and the anomaly is WARNed, never silent.
func TestContributionsEngineOptions_NilPayloadIsUnresolvedNotLocal(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	opts := contributionsEngineOptions(nil, logger)
	if len(opts) != 1 {
		t.Fatalf("expected exactly one engine option for a nil payload, got %d", len(opts))
	}
	if !strings.Contains(buf.String(), "contributions payload") {
		t.Errorf("the anomaly must be WARNed, never silent; log = %q", buf.String())
	}
}

func TestContributionsEngineOptions_PayloadRidesAuthoritative(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	opts := contributionsEngineOptions(&queue.Contributions{
		Library: []queue.LibrarySkillFile{{Name: "s", Content: []byte("body")}},
	}, logger)
	if len(opts) != 1 {
		t.Fatalf("expected exactly one engine option for a carried payload, got %d", len(opts))
	}
	if buf.String() != "" {
		t.Errorf("a carried payload is not an anomaly; unexpected log: %q", buf.String())
	}
}

// Wiring witness, in the house style of the prune-gate count test: BOTH
// dispatch paths (root run + subbot child) must translate the message's
// contributions through the ONE helper. A site reverting to the bare
// nil-guard reintroduces the R6 defect on that path only — invisible to the
// helper-level tests above.
func TestContributionsEngineOptions_WiredOnBothDispatchPaths(t *testing.T) {
	sites := 0
	for _, f := range []string{"loop.go", "subbot.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sites += strings.Count(string(src), "contributionsEngineOptions(")
	}
	if sites != 2 {
		t.Fatalf("expected contributionsEngineOptions wired on exactly 2 dispatch paths (loop.go + subbot.go), found %d", sites)
	}
}

// The publisher's degraded confession rides the wire and lands in the
// engine's domain type: a payload built from a partially-enumerated instance
// must keep that fact through the conversion, or the pod-side veto
// (TestMirrorPluginContributions_DegradedPayloadFlagsIncomplete) starves.
func TestContributionsFromWire_CarriesDegraded(t *testing.T) {
	got := contributionsFromWire(&queue.Contributions{
		Plugin:   []queue.ContributionFile{{Kind: "skills", Name: "a.md", Content: []byte("x")}},
		Degraded: true,
	})
	if !got.Degraded {
		t.Error("Degraded lost in the wire→domain conversion")
	}
}

// The nil-payload option must land as a REAL effect on the engine, not just
// exist in the returned slice: apply the helper's options and assert the
// engine state. Closes the composition joint the re-attack round proved open
// (an option that did nothing but WARN survived every committed test).
func TestContributionsEngineOptions_NilPayloadEffectOnEngine(t *testing.T) {
	e := &runtime.Engine{}
	for _, opt := range contributionsEngineOptions(nil, nil) {
		opt(e)
	}
	if !e.ContributionsUnresolved() {
		t.Fatal("nil payload must flag the engine's ambient declaration unresolved")
	}
}
