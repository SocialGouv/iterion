package runtime

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/stdiotest"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
)

// The output-validation dial has three positions, and unset is not off: an
// operator who never set it gets the refusal of a tool recipe's non-JSON
// stdout, and one who wrote the off value out gets the wrapper back.
func TestTheOutputValidationDialTellsUnsetFromOff(t *testing.T) {
	cases := []struct {
		value                  string
		strict, stdoutIsJudged bool
	}{
		{"", false, true},
		{"  ", false, true},
		{"1", true, true},
		{"true", true, true},
		{"on", true, true},
		{"0", false, false},
		{"false", false, false},
		{"off", false, false},
		{" No ", false, false},
		{"disabled", false, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q", c.value), func(t *testing.T) {
			t.Setenv(EnvOutputValidation, c.value)
			t.Setenv(EnvOutputRefs, "")
			e := New(validationWorkflow(), tmpStore(t), newStubExecutor())
			if e.validateOutputs != c.strict {
				t.Errorf("strict = %v, want %v", e.validateOutputs, c.strict)
			}
			contract := model.ToolContractFromContext(e.templateContext(context.Background(), e.newRunState("dial", nil), resolveScope{}))
			if contract.JSONStdout != c.stdoutIsJudged {
				t.Errorf("a tool recipe's stdout is judged = %v, want %v", contract.JSONStdout, c.stdoutIsJudged)
			}
			if !contract.OutputRefs {
				t.Error("the reference dial moved with the validation dial")
			}
		})
	}

	t.Run("a typo reads as unset, and says so", func(t *testing.T) {
		t.Setenv(EnvOutputValidation, "flase")
		var dial outputValidationDial
		warned := stdiotest.Run(t, &os.Stderr, func() { dial = outputValidationFromEnv() })
		if dial != outputValidationDefault {
			t.Errorf("dial = %v, want the default: a typo must not switch the refusal off", dial)
		}
		if !strings.Contains(warned, "OUTPUT_VALIDATION_INVALID") {
			t.Errorf("no warning for a value that is not a boolean: %q", warned)
		}
	})

	t.Run("the engine option is the dial set by hand, over the environment", func(t *testing.T) {
		t.Setenv(EnvOutputValidation, "1")
		off := New(validationWorkflow(), tmpStore(t), newStubExecutor(), WithOutputValidation(false))
		if off.validateOutputs || !off.toolStdoutWrapped {
			t.Errorf("WithOutputValidation(false): strict=%v wrapped=%v, want the explicit off", off.validateOutputs, off.toolStdoutWrapped)
		}
		t.Setenv(EnvOutputValidation, "0")
		on := New(validationWorkflow(), tmpStore(t), newStubExecutor(), WithOutputValidation(true))
		if !on.validateOutputs || on.toolStdoutWrapped {
			t.Errorf("WithOutputValidation(true): strict=%v wrapped=%v, want strict", on.validateOutputs, on.toolStdoutWrapped)
		}
	})
}

// The reference dial is on unless it is written out as off; a typo keeps it
// on and says so.
func TestTheOutputRefsDialIsOnUnlessWrittenOff(t *testing.T) {
	for value, want := range map[string]bool{"": true, " ": true, "1": true, "on": true, "0": false, "false": false, "off": false, "disabled": false} {
		t.Setenv(EnvOutputRefs, value)
		if got := defaultOutputRefsOwed(); got != want {
			t.Errorf("%s=%q: owed = %v, want %v", EnvOutputRefs, value, got, want)
		}
		t.Setenv(EnvOutputValidation, "")
		e := New(validationWorkflow(), tmpStore(t), newStubExecutor())
		contract := model.ToolContractFromContext(e.templateContext(context.Background(), e.newRunState("dial", nil), resolveScope{}))
		if contract.OutputRefs != want || !contract.JSONStdout {
			t.Errorf("%s=%q: the dispatch context carries %+v", EnvOutputRefs, value, contract)
		}
	}
	t.Setenv(EnvOutputRefs, "nope")
	var got bool
	warned := stdiotest.Run(t, &os.Stderr, func() { got = defaultOutputRefsOwed() })
	if !got || !strings.Contains(warned, "OUTPUT_REFS_INVALID") {
		t.Errorf("a typo: owed=%v warned=%q, want the default kept and a warning", got, warned)
	}
}

// The names the refusals print are the names the engine reads.
func TestTheToolContractMessagesNameTheDialsTheEngineReads(t *testing.T) {
	if EnvOutputValidation != "ITERION_STRICT_OUTPUT_VALIDATION" || EnvOutputRefs != "ITERION_STRICT_OUTPUT_REFS" {
		t.Fatalf("the dials are %q and %q", EnvOutputValidation, EnvOutputRefs)
	}
	if msg := (&model.ToolStdoutNotJSONError{Schema: "s"}).Error(); !strings.Contains(msg, EnvOutputValidation+"=0") {
		t.Errorf("the stdout refusal does not name its dial: %s", msg)
	}
	if msg := (&model.UnresolvedOutputRefError{Surface: "command"}).Error(); !strings.Contains(msg, EnvOutputRefs+"=0") {
		t.Errorf("the reference refusal does not name its dial: %s", msg)
	}
}

// A tool-contract refusal is a permanent tool failure wherever it is
// classified — through the tool-failure wrapper every tool node's error
// travels in, and whatever words the message carries.
func TestAToolContractViolationClassifiesAsAPermanentToolFailure(t *testing.T) {
	violations := map[string]error{
		"stdout that is not JSON": &model.ToolStdoutNotJSONError{Schema: "publish_out", Bytes: 92},
		"an owed output reference": &model.UnresolvedOutputRefError{Surface: "command", Owed: []model.OwedOutputRef{
			// A producer named after a transport failure: the network needle
			// of the tool-failure arm must not get to read it.
			{Ref: "outputs.connection_reset.id", Producer: "connection reset by peer", Schema: "s", Field: "id"},
		}},
	}
	for name, violation := range violations {
		t.Run(name, func(t *testing.T) {
			err := &model.ToolExecutionError{Cause: fmt.Errorf("model: tool node %q: %w", "n", violation)}
			code := ClassifyError(err)
			if code != ErrCodeToolFailedPermanent {
				t.Fatalf("classified %s, want %s", code, ErrCodeToolFailedPermanent)
			}
			if !retrypolicy.IsDeterministic(code) {
				t.Errorf("%s is not deterministic for the retry policy: the run would be redelivered", code)
			}
		})
	}
}
