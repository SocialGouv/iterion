package kubernetes

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	"os/exec"
	"strings"
	"testing"
)

func TestNativeExecTransportRejectedConstructionPreservesCause(t *testing.T) {
	sentinel := errors.New("original transport refusal")
	t.Run("pi", func(t *testing.T) {
		c := pisdk.NewClient(pisdk.ClientOptions{PreserveCommandCancellation: true, Spawn: func(context.Context, []string) *exec.Cmd { return &exec.Cmd{Err: sentinel} }})
		err := c.Start(t.Context())
		if !errors.Is(err, sentinel) {
			t.Fatalf("constructor cause lost: %v", err)
		}
	})
	t.Run("claude", func(t *testing.T) {
		_, err := claudesdk.Prompt(t.Context(), "no model", claudesdk.WithPreservedCommandCancellation(true), claudesdk.WithCommandBuilder(func(context.Context, string, []string, string, map[string]string, bool) *exec.Cmd {
			return &exec.Cmd{Err: sentinel}
		}))
		if !errors.Is(err, sentinel) {
			t.Fatalf("constructor cause lost: %v", err)
		}
	})
	t.Run("pi_missing_hook", func(t *testing.T) {
		c := pisdk.NewClient(pisdk.ClientOptions{PreserveCommandCancellation: true, Spawn: func(context.Context, []string) *exec.Cmd { return exec.Command("/bin/true") }})
		err := c.Start(t.Context())
		if err == nil || !strings.Contains(err.Error(), "Cancel hook") {
			t.Fatalf("executable without hook accepted: %v", err)
		}
	})
	t.Run("claude_missing_hook", func(t *testing.T) {
		_, err := claudesdk.Prompt(t.Context(), "no model", claudesdk.WithPreservedCommandCancellation(true), claudesdk.WithCommandBuilder(func(context.Context, string, []string, string, map[string]string, bool) *exec.Cmd {
			return exec.Command("/bin/true")
		}))
		if err == nil || !strings.Contains(err.Error(), "Cancel hook") {
			t.Fatalf("executable without hook accepted: %v", err)
		}
	})

}
