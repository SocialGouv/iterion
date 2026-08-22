package runview

import (
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
		if reason := ir.UngatedCrossingReason(backend, permission); reason != "" {
			return fmt.Errorf("runview: model override for %s %q %s", llm.NodeKind(), llm.NodeID(), reason)
		}
	}
	return nil
}

func firstPermission(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
