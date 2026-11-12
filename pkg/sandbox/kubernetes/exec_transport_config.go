package kubernetes

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

const execTransportPrefix = "ITERION_SANDBOX_K8S_EXEC_"
const defaultGuestHelper = "/usr/local/libexec/iterion-execenv-capsule"
const defaultExecStartTimeout = 30 * time.Second

// execTransportConfig contains only captured deployment authority. It is copied
// with Driver and never resolved again from workload Env or at command Start.
type execTransportConfig struct {
	hostHelper, guestHelper string
	profile                 protocol.ExecutionProfile
	startTimeout            time.Duration
}

// loadExecTransportConfig is called once by New. Its error is retained on the
// driver, not returned to the factory: Env-empty commands need no guest helper.
func loadExecTransportConfig() (execTransportConfig, error) {
	var zero execTransportConfig
	suffixes := [...]string{"HOST_HELPER", "GUEST_HELPER", "PROTOCOL", "DEFAULT_PATH", "SHELL", "START_TIMEOUT"}
	values := make(map[string]string, len(suffixes))
	for _, suffix := range suffixes {
		key := execTransportPrefix + suffix
		if envtrust.Planted(key) {
			return zero, execConfigRefusal("operator-provenance", nil)
		}
		if value, present := os.LookupEnv(key); present {
			values[suffix] = value
		}
	}
	cfg := execTransportConfig{
		guestHelper:  defaultGuestHelper,
		profile:      protocol.ExecutionProfile{DefaultPath: "/bin:/usr/bin", Shell: "/bin/sh"},
		startTimeout: defaultExecStartTimeout,
	}
	if host, present := values["HOST_HELPER"]; present {
		cfg.hostHelper = host
	} else {
		host, err := envtrust.LookPath("iterion")
		if err != nil {
			return zero, execConfigRefusal("host-helper", err)
		}
		cfg.hostHelper = host
	}
	if !filepath.IsAbs(cfg.hostHelper) || strings.IndexByte(cfg.hostHelper, 0) >= 0 {
		return zero, execConfigRefusal("host-helper", nil)
	}
	if guest, present := values["GUEST_HELPER"]; present {
		cfg.guestHelper = guest
	}
	if !path.IsAbs(cfg.guestHelper) || strings.IndexByte(cfg.guestHelper, 0) >= 0 {
		return zero, execConfigRefusal("guest-helper", nil)
	}
	if version, present := values["PROTOCOL"]; present && version != "2" {
		return zero, execConfigRefusal("protocol", nil)
	}
	if value, present := values["DEFAULT_PATH"]; present {
		cfg.profile.DefaultPath = value
	}
	if value, present := values["SHELL"]; present {
		cfg.profile.Shell = value
	}
	if raw, present := values["START_TIMEOUT"]; present {
		duration, err := time.ParseDuration(raw)
		if err != nil || duration < 0 {
			return zero, execConfigRefusal("startup-timeout", nil)
		}
		cfg.startTimeout = duration
	}
	// Delegate native byte validation to the same codec used at execution.
	// Discard has no I/O effects; no second profile validator can drift here.
	if err := protocol.WriteInvocation(io.Discard, protocol.Invocation{Argv: []string{"profile-validation"}, Profile: &cfg.profile}); err != nil {
		return zero, err
	}
	return cfg, nil
}

func execConfigRefusal(kind string, cause error) error {
	return &protocol.Error{Phase: "kubernetes-config", Kind: kind, Cause: cause}
}
