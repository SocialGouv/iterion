package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestRuntimeValidatesOutputsByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	eng := New(validationWorkflow(), tmpStore(t), newStubExecutor())
	err := eng.Run(context.Background(), "validation-default", nil)
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeSchemaValidation {
		t.Fatalf("default engine accepted invalid output: %v", err)
	}
}
