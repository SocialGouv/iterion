package runview

import (
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ValidateModelOverridePermissions rejects a launch-time backend override
// that would silently remove a node's effective tool-permission gate.
//
// BuildExecutor is the common choke point for CLI launch/resume, local Studio
// and the cloud runner. The cloud publisher also calls this before queueing so
// the operator gets a synchronous admission error rather than a remotely
// failed run. The backend predicate is shared with C176 and --fallback.
func ValidateModelOverridePermissions(wf *ir.Workflow, overrides model.ModelOverrides, runPermission string) error {
	if wf == nil || overrides.Empty() {
		return nil
	}
	for _, node := range wf.Nodes {
		llm, ok := node.(ir.LLMNode)
		if !ok {
			continue
		}
		backend := overrides.ForNode(llm.NodeID(), llm.NodeKind()).Backend
		if backend == "" {
			continue
		}
		permission := firstPermission(runPermission, llm.GetPermission(), wf.Permission, os.Getenv("ITERION_PERMISSION"))
		if reason := ir.UngatedCrossingReasonForAskRules(backend, permission, ir.EffectiveAskRules(llm, wf)); reason != "" {
			return fmt.Errorf("runview: model override for %s %q %s", llm.NodeKind(), llm.NodeID(), reason)
		}
	}
	return nil
}

// ScreenPolicyOverrides compiles a bot the way a launch compiles it and
// reports, per LLM node, whether the routing block's overrides would
// re-target it onto a backend that cannot gate the node's effective
// ask-rules — the SAME predicate the launch enforces as a refusal
// (ValidateModelOverridePermissions), collected per node instead of
// failing on the first. It backs the write-surface WARNING (C1xx posture):
// a `*` cell reaches every bot the integration fans out to, and a block
// that would refuse a co-enabled bot's webhook delivery must say so at
// save time, not at the first delivery after it. NOT an enforcement — a
// bot enabled after the write is never screened, and the launch refusal
// above stays the gate.
func ScreenPolicyOverrides(path, source, bundleDir string, pol *store.RunLLMRoutePolicy) ([]string, error) {
	if pol == nil || len(pol.Overrides) == 0 {
		return nil, nil
	}
	wf, _, _, err := compileForLaunch(path, source, bundleDir)
	if err != nil {
		return nil, err
	}
	if wf == nil {
		return nil, nil
	}
	overrides := toModelOverrides(EffectiveModelOverrides(nil, pol))
	var warnings []string
	for _, node := range wf.Nodes {
		llm, ok := node.(ir.LLMNode)
		if !ok {
			continue
		}
		backend := overrides.ForNode(llm.NodeID(), llm.NodeKind()).Backend
		if backend == "" {
			continue
		}
		permission := firstPermission("", llm.GetPermission(), wf.Permission, os.Getenv("ITERION_PERMISSION"))
		if reason := ir.UngatedCrossingReasonForAskRules(backend, permission, ir.EffectiveAskRules(llm, wf)); reason != "" {
			warnings = append(warnings, fmt.Sprintf("the routing block moves %s %q onto a backend that cannot gate it: %s", llm.NodeKind(), llm.NodeID(), reason))
		}
	}
	return warnings, nil
}

func firstPermission(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
