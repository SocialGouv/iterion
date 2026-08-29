package cli

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// A CLI-launched run must record what the operator asked for, or the resume
// inheritance is inert on exactly the path that has no other surface to fall
// back on: the studio Overview shows no override, and `iterion resume` has
// nothing to read back, so the run silently reverts to the .bot's own values.
func TestRunModelOverrideRows_StampsWhatWasParsed(t *testing.T) {
	ov, err := model.ParseModelOverrides(
		[]string{"reviewer_*=anthropic/claude-opus-5"},
		[]string{"*=claw"},
		[]string{"fix_*=max"},
	)
	if err != nil {
		t.Fatalf("ParseModelOverrides: %v", err)
	}
	rows := runModelOverrideRows(ov)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(rows), rows)
	}
	if r := rows[0]; r.Selector != "reviewer_*" || r.Model != "anthropic/claude-opus-5" {
		t.Errorf("rows[0] = %+v", r)
	}
	if r := rows[1]; r.Selector != "*" || r.Backend != "claw" {
		t.Errorf("rows[1] = %+v", r)
	}
	if r := rows[2]; r.Selector != "fix_*" || r.Effort != "max" {
		t.Errorf("rows[2] = %+v", r)
	}
}

func TestRunModelOverrideRows_EmptyStaysNil(t *testing.T) {
	if rows := runModelOverrideRows(model.ModelOverrides{}); rows != nil {
		t.Errorf("rows = %+v, want nil so the engine's no-override path keeps its meaning", rows)
	}
}
