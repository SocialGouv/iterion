package docker

import (
	"fmt"
	"os"
)

// proxyStartEnvFile keeps the run's egress token out of both the runtime
// client's argv and its environment. The latter configures the client's own
// HTTP transport, so a workload proxy must never be placed there. Start owns
// this 0600 file through the create/retry phase and removes it on every
// return path; docker/podman read it before the container is created.
func proxyStartEnvFile(endpoint string) (string, error) {
	for _, name := range proxyEnvNames {
		if err := validateEnvVar(name, endpoint); err != nil {
			return "", err
		}
	}
	f, err := os.CreateTemp("", "iterion-proxy-env-*")
	if err != nil {
		return "", fmt.Errorf("docker: create private proxy environment: %w", err)
	}
	path := f.Name()
	complete := false
	defer func() {
		_ = f.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	for _, name := range proxyEnvNames {
		if _, err := fmt.Fprintf(f, "%s=%s\n", name, endpoint); err != nil {
			return "", fmt.Errorf("docker: write private proxy environment: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("docker: close private proxy environment: %w", err)
	}
	complete = true
	return path, nil
}
