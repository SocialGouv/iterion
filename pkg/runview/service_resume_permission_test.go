package runview

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestResumeExecutorSpecReplaysPermissionOverride(t *testing.T) {
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	spec := svc.resumeExecutorSpec(&ir.Workflow{Name: "wf", Permission: "off"}, &store.Run{
		ID: "run-1", PermissionOverride: "deny",
	}, iterlog.Nop(), "")
	if spec.Permission != "deny" {
		t.Fatalf("resume Permission = %q, want original run-level deny", spec.Permission)
	}
}
