package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRuntimeWarnsAboutInvalidOutputsByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(EnvOutputValidation, "")
	st := tmpStore(t)
	eng := New(validationWorkflow(), st, newStubExecutor())
	if err := eng.Run(context.Background(), "validation-default", nil); err != nil {
		t.Fatalf("default engine rejected advisory schema mismatch: %v", err)
	}
	events, err := st.LoadEvents(context.Background(), "validation-default")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == store.EventNodeOutputSchemaMismatch {
			return
		}
	}
	t.Fatal("default engine omitted the schema warning")
}
