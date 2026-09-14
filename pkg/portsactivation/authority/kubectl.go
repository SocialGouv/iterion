package authority

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

type boundedKubectlOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedKubectlOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, errors.New("Kubernetes response exceeds supported size")
	}
	return b.buffer.Write(p)
}

// runKubectl is only a bounded read transport. Every caller validates literal
// resource names and checks the returned object's identity and shape. It never
// exposes stderr because kubectl diagnostics can quote Secret or env content.
func runKubectl(ctx context.Context, kubectlBinary string, args []string, limit int) ([]byte, error) {
	if kubectlBinary == "" || limit <= 0 {
		return nil, fmt.Errorf("Kubernetes read transport is unavailable")
	}
	requestContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(requestContext, kubectlBinary, args...)
	command.WaitDelay = time.Second
	output := &boundedKubectlOutput{limit: limit}
	command.Stdout = output
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("Kubernetes read transport failed")
	}
	return bytes.Clone(output.buffer.Bytes()), nil
}
