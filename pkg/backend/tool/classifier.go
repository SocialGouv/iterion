package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/permissions"
)

// ClassifierLogger is the minimal logging surface ClassifierChecker uses to
// surface classifier errors and malformed inputs without coupling the tool
// package to a concrete logger. Both methods accept printf-style arguments.
//
// log.Logger satisfies this interface implicitly via its Warn/Debug methods.
type ClassifierLogger interface {
	Warn(format string, args ...any)
	Debug(format string, args ...any)
}

// ClassifierChecker wraps a permissions.Classifier (typically LLMClassifier
// chained over RuleClassifier) into iterion's ToolChecker interface. The
// classifier is consulted first and short-circuits on Allow/Deny; Ask
// falls through to the optional base checker.
//
// A nil ClassifierChecker.Base treats Ask as Allow (the static policy is
// optional). Use BuildChecker as the base to keep workflow-level allowlists
// and per-node overrides intact while letting an LLMClassifier veto or
// pre-approve calls.
//
// Logger is optional; when set, classifier errors and JSON decoding
// failures are surfaced at Warn level. Without a logger, errors fall
// through silently to the base checker (legacy behavior).
type ClassifierChecker struct {
	Classifier permissions.Classifier
	Base       ToolChecker
	Logger     ClassifierLogger
}

// CheckContext implements ToolChecker.
func (cc *ClassifierChecker) CheckContext(pctx PolicyContext) error {
	if cc == nil || cc.Classifier == nil {
		if cc != nil && cc.Base != nil {
			return cc.Base.CheckContext(pctx)
		}
		return nil
	}

	args, decodeErr := decodeArgs(pctx.Input)
	if decodeErr != nil && cc.Logger != nil {
		cc.Logger.Warn("tool classifier: malformed input for %q (node=%q): %v", pctx.ToolName, pctx.NodeID, decodeErr)
	}

	// Honour the caller's context for cancellation/deadlines. Falls back to
	// Background only when the caller did not populate one.
	ctx := pctx.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	d, err := cc.Classifier.Classify(ctx, pctx.ToolName, args)
	if err != nil {
		if cc.Logger != nil {
			cc.Logger.Warn("tool classifier: error classifying %q (node=%q): %v", pctx.ToolName, pctx.NodeID, err)
		}
	} else {
		switch d {
		case permissions.DecisionAllow:
			if cc.Logger != nil {
				cc.Logger.Debug("tool classifier: allow %q (node=%q)", pctx.ToolName, pctx.NodeID)
			}
			return nil
		case permissions.DecisionDeny:
			if cc.Logger != nil {
				cc.Logger.Debug("tool classifier: deny %q (node=%q)", pctx.ToolName, pctx.NodeID)
			}
			return fmt.Errorf("%w: classifier denied %q", ErrToolDenied, pctx.ToolName)
		}
	}
	// DecisionAsk or classifier error → defer to the base checker, or
	// allow when no base is configured (the LLM did not refuse).
	if cc.Base != nil {
		return cc.Base.CheckContext(pctx)
	}
	return nil
}

// decodeArgs parses tool arguments from a JSON-encoded payload. Returns
// (nil, nil) when the payload is empty (no arguments provided), and
// (nil, err) when the payload is non-empty but malformed — callers can
// log the error and fall through to a permissive default.
func decodeArgs(input json.RawMessage) (map[string]any, error) {
	if len(input) == 0 {
		return nil, nil
	}
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("decode tool args: %w", err)
	}
	return args, nil
}
