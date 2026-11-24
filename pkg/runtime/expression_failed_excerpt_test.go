package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The EXPRESSION_FAILED message quoted the whole expression: at the read
// budget's edge that is a megabyte of source in one failure line (#2775).
// The runtime quotes the 64-rune excerpt, as the compiler's diagnostics do.
func TestExpressionFailedQuotesTheExpressionByExcerpt(t *testing.T) {
	long := "'" + strings.Repeat("x", 400) + "' - 1"
	wf := computeGaugeWorkflow(t, ir.FieldTypeString, long)
	err := New(wf, tmpStore(t), newStubExecutor()).Run(context.Background(), "run-excerpt", nil)
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeExpressionFailed {
		t.Fatalf("expected a %s error, got %v", ErrCodeExpressionFailed, err)
	}
	if strings.Contains(rtErr.Message, long) {
		t.Errorf("the message echoes the whole %d-byte expression", len(long))
	}
	if !strings.Contains(rtErr.Message, expr.Excerpt(long)) {
		t.Errorf("the message does not quote the excerpt: %q", rtErr.Message)
	}
	if len(rtErr.Message) > 512 {
		t.Errorf("the message is %d bytes, want a bounded line", len(rtErr.Message))
	}
}
