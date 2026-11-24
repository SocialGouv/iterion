package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestExecuteOracleFallsBackToSessionModel(t *testing.T) {
	loop := &ConversationLoop{
		Client: &stubStreamClient{text: "Recommendation: split the package."},
		Config: &Config{Model: "session-model"},
	}
	out, err := loop.executeOracle(context.Background(), map[string]any{"question": "split?"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[oracle · session-model]") || !strings.Contains(out, "Recommendation") {
		t.Errorf("oracle output wrong: %q", out)
	}

	loop.Config.OracleModel = "advisor-model"
	out, err = loop.executeOracle(context.Background(), map[string]any{"question": "split?"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[oracle · advisor-model]") {
		t.Errorf("OracleModel override not honored: %q", out)
	}
}

func TestExecuteOracleValidation(t *testing.T) {
	loop := &ConversationLoop{Client: &stubStreamClient{text: "x"}, Config: &Config{}}
	if _, err := loop.executeOracle(context.Background(), map[string]any{}); err == nil {
		t.Error("missing question must error")
	}
	loop.Client = nil
	if _, err := loop.executeOracle(context.Background(), map[string]any{"question": "q"}); err == nil {
		t.Error("nil client must error")
	}
}
