package runview

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

type outputReadRefusal struct {
	store.RunStore
	artifact bool
	partial  bool
}

func (s outputReadRefusal) Unwrap() store.RunStore { return s.RunStore }
func (s outputReadRefusal) ScanEvents(ctx context.Context, id string, visit func(*store.Event) bool) error {
	if s.artifact {
		return nil
	}
	if s.partial {
		visit(&store.Event{Type: store.EventNodeFinished, NodeID: "node", Data: map[string]any{"output": map[string]any{"value": "partial"}}})
	}
	return store.ErrValueFormatChanged
}
func (s outputReadRefusal) LoadLatestArtifact(context.Context, string, string) (*store.Artifact, error) {
	return nil, store.ErrValueFormatChanged
}

func TestSubbotOutputReadRefusalKeepsAttachment(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		contract, partial, artifact bool
	}{
		{name: "scan"}, {name: "partial scan", partial: true},
		{name: "contract scan", contract: true}, {name: "partial contract scan", contract: true, partial: true},
		{name: "artifact", artifact: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := mustStore(t)
			if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
				t.Fatal(err)
			}
			mkChild(t, s, "parent", "node", "child", store.RunStatusFinished, map[string]any{"value": "original"})
			child, err := s.LoadRun(ctx, "child")
			if err != nil {
				t.Fatal(err)
			}
			if tc.contract {
				child.PublicContract = []byte(`{}`)
			}
			if tc.artifact {
				child.ArtifactIndex = map[string]int{"node": 0}
			}
			if err := s.SaveRun(ctx, child); err != nil {
				t.Fatal(err)
			}
			if err := s.SetSubbotChild(ctx, "parent", "key", "child"); err != nil {
				t.Fatal(err)
			}
			out, err, handled := ReattachSubbotChild(ctx, outputReadRefusal{RunStore: s, partial: tc.partial, artifact: tc.artifact}, runtime.SubbotRequest{ParentRunID: "parent", NodeID: "node", ReattachKey: "key"}, nil)
			parent, loadErr := s.LoadRun(ctx, "parent")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if !handled || !errors.Is(err, store.ErrValueFormatChanged) || out != nil || parent.SubbotChildren["key"] != "child" {
				t.Fatalf("read refusal lost: handled=%v output=%v err=%v retained=%q", handled, out, err, parent.SubbotChildren["key"])
			}
		})
	}
}
