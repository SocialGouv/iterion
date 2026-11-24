package execenv

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

const HostMode = "__exec-env-host-v1"

// Launch contains already-resolved operator authority. No value is resolved
// from Invocation.Env. StartTimeout/WaitDelay are resolved before construction;
// zero explicitly means unbounded. The configuration boundary owns warnings and
// the existing ITERION_PROC_WAIT_DELAY policy, not this pure constructor.
type Launch struct {
	HostHelper, ClientPath  string
	ClientArgs, ControlEnv  []string
	Invocation              protocol.Invocation
	StartTimeout, WaitDelay time.Duration
}

// NewCommand copies/encodes the launch without filesystem I/O, starting a
// process or goroutine. A nil stdin remains available for the SDK's StdinPipe.
// It does not assume that os.Executable is an iterion binary. Native exec size
// limits still apply to the encoded environment at Start.
func NewCommand(ctx context.Context, l Launch, stdin io.Reader) *exec.Cmd {
	// Refusals must be constructed without exec.Command: a relative helper
	// would trigger LookPath before our validation, and other platforms resolve
	// extensions. No rejected configuration may perform that lookup.
	cmd := &exec.Cmd{Path: l.HostHelper, Args: []string{l.HostHelper, HostMode}, Stdin: stdin}
	if !filepath.IsAbs(l.HostHelper) || strings.IndexByte(l.HostHelper, 0) >= 0 {
		cmd.Err = refusal("host", "helper-path", nil)
		return cmd
	}
	if err := hostSupported(); err != nil {
		cmd.Err = err
		return cmd
	}
	raw, err := encodeWire(wireLaunch{ClientPath: l.ClientPath, ClientArgs: l.ClientArgs, Invocation: l.Invocation, StartTimeout: l.StartTimeout, WaitDelay: l.WaitDelay})
	if err != nil {
		cmd.Err = err
		return cmd
	}
	environment, err := wireEnvironment(l.ControlEnv, raw)
	if err != nil {
		cmd.Err = err
		return cmd
	}
	cmd = exec.CommandContext(ctx, l.HostHelper, HostMode)
	cmd.Stdin = stdin
	cmd.Env = environment
	cmd.WaitDelay = l.WaitDelay
	configureHostCommand(cmd)
	return cmd
}
