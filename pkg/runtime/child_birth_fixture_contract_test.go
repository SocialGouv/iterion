package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

type childBirthOpaqueStore struct{ store.RunStore }

func TestChildBirthWrappersExposeOnlyBirthCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(store.RunStore) store.RunStore
	}{
		{"share record refusal", func(base store.RunStore) store.RunStore { return &shareRecordRefused{RunStore: base, times: 1} }},
		{"parent creation recorder", func(base store.RunStore) store.RunStore { return &recordingParentStore{RunStore: base} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tmpStore(t)
			if _, err := base.CreateRun(t.Context(), "parent", "fixture", nil); err != nil {
				t.Fatal(err)
			}
			if store.AsRunExecutionStore(base) == nil {
				t.Fatal("control: underlying store lacks execution capability")
			}
			wrapped := tc.wrap(base)
			if store.AsRunValueFormatCreator(wrapped) == nil {
				t.Fatal("BIRTH_CAPABILITY_MASKED")
			}
			if store.AsRunExecutionStore(wrapped) != nil {
				t.Fatal("UNRELATED_EXECUTION_CAPABILITY_EXPOSED")
			}
			child, err := store.CreateRunWithValueFormat(t.Context(), wrapped, store.RunCreateSpec{ID: "child", ParentRunID: "parent", Format: store.LegacyValueFormat})
			if err != nil || child.ParentRunID != "parent" || child.FormatVersion != 1 {
				t.Fatalf("BIRTH_FACTS child=%+v err=%v", child, err)
			}
			if refusing, ok := wrapped.(*shareRecordRefused); ok {
				if _, err = refusing.AppendEvent(t.Context(), "child", store.Event{Type: store.EventSandboxShared}); err == nil || refusing.refused != 1 {
					t.Fatalf("REFUSAL_WRAPPER_BYPASSED err=%v count=%d", err, refusing.refused)
				}
			}
			// An opaque delegate has no birth port. Forwarding may not silently lose
			// the parent or create/save through the legacy API to fake that capability.
			opaque := tc.wrap(childBirthOpaqueStore{RunStore: tmpStore(t)})
			_, err = store.CreateRunWithValueFormat(t.Context(), opaque, store.RunCreateSpec{ID: "unsupported", ParentRunID: "parent", Format: store.LegacyValueFormat})
			if !errors.Is(err, store.ErrValueFormatCapability) {
				t.Fatalf("OPAQUE_DELEGATE_ACCEPTED %v", err)
			}
			if _, err = opaque.LoadRun(t.Context(), "unsupported"); !errors.Is(err, store.ErrRunNotFound) {
				t.Fatalf("UNSUPPORTED_CHILD_CREATED %v", err)
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = store.CreateRunWithValueFormat(canceled, wrapped, store.RunCreateSpec{ID: "canceled", ParentRunID: "parent", Format: store.LegacyValueFormat})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("CANCEL_LOST %v", err)
			}
			if _, err = base.LoadRun(t.Context(), "canceled"); !errors.Is(err, store.ErrRunNotFound) {
				t.Fatalf("CANCELED_CHILD_CREATED %v", err)
			}
		})
	}
}
