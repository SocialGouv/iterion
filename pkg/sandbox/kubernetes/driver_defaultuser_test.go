package kubernetes

import (
	"context"
	"io"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// A spec without user: (the platform's synthetic default-image spec under
// sandbox-by-default) defaults to the published images' devbox uid instead
// of hard-failing at boot.
func TestPrepareDefaultsUser(t *testing.T) {
	d := &Driver{kubectl: "kubectl", namespace: "test", logger: iterlog.New(iterlog.LevelInfo, io.Discard)}
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode:      sandbox.ModeAuto,
		Image:     "ghcr.io/socialgouv/iterion-sandbox-full:edge",
		HostState: sandbox.HostStateNone,
	})
	if err != nil {
		t.Fatalf("Prepare must default the user, got: %v", err)
	}
	if got := prepared.(*Prepared).spec.User; got != defaultPodUser {
		t.Fatalf("User = %q, want %q", got, defaultPodUser)
	}
	// An explicit user still wins.
	prepared, err = d.Prepare(context.Background(), sandbox.Spec{
		Mode:      sandbox.ModeAuto,
		Image:     "ghcr.io/socialgouv/iterion-sandbox-full:edge",
		HostState: sandbox.HostStateNone,
		User:      "1234",
	})
	if err != nil {
		t.Fatalf("explicit user: %v", err)
	}
	if got := prepared.(*Prepared).spec.User; got != "1234" {
		t.Fatalf("User = %q, want 1234", got)
	}
	// Non-numeric user still rejected by ValidateSpec.
	if _, err = d.Prepare(context.Background(), sandbox.Spec{
		Mode: sandbox.ModeAuto, Image: "x", HostState: sandbox.HostStateNone, User: "devbox",
	}); err == nil || !strings.Contains(err.Error(), "must be numeric") {
		t.Fatalf("non-numeric user must be rejected, got: %v", err)
	}
}
