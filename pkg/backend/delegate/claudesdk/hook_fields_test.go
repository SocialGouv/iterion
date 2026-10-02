package claudesdk

import (
	"reflect"
	"testing"
)

// A PostToolUse hook's replacement output travels as updatedToolOutput; a
// no-op hook sets no field at all (the CLI rejects a bare hookEventName).
func TestHookSpecificFieldsCarryAReplacedToolOutput(t *testing.T) {
	out := map[string]any{"command": "__P__"}
	if got := hookSpecificFields(HookOutput{UpdatedToolOutput: out}); !reflect.DeepEqual(got, map[string]any{"updatedToolOutput": out}) {
		t.Fatalf("fields = %#v", got)
	}
	if got := hookSpecificFields(HookOutput{}); len(got) != 0 {
		t.Fatalf("a no-op hook sets %#v", got)
	}
}
