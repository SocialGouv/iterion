package delegate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
)

// ambientHostNotices remembers, per backend and file, that the default policy
// was reported unable to withhold a host instruction file, so the operator is
// told once per process rather than on every node.
var ambientHostNotices sync.Map

// applyAmbient serves a CLI that reads operator instruction files (Protocol.
// Ambient) what a policy without the operator origin allows: on the host, an
// overlay of its home without those files, pointed at through the task's
// ExtraEnv, which both spawn paths apply last. The returned release carries
// back what the CLI wrote into the overlay and must run once the invocation
// is done.
//
// A host file the overlay cannot withhold refuses a declared policy with a
// *ErrCapabilityUnsupported — running would hand the agent the very file the
// policy excludes — and is reported once under the default, which each
// backend applies as far as it can.
func (b *CLIAgentBackend) applyAmbient(task Task, backendName string) (Task, func(), error) {
	amb := b.Protocol.Ambient
	noop := func() {}
	if amb == nil || task.AmbientContext.IncludesOperator() || !task.Hostless() {
		return task, noop, nil
	}
	if amb.HostInstructions != nil {
		if files := amb.HostInstructions(task); len(files) > 0 {
			reason := fmt.Sprintf("%s reads %s, outside its own home, and nothing keeps it out", backendName, strings.Join(files, ", "))
			if ambient.Declared(task.AmbientContextSource) {
				return task, nil, &ErrCapabilityUnsupported{
					NodeID:     task.NodeID,
					Backend:    backendName,
					Capability: ambient.Capability(task.AmbientContext, task.AmbientContextSource),
					Remedy: fmt.Sprintf("%s; move it, declare ambient_context: all, or run the node on a backend that applies %s (%s) — ADR-119",
						reason, task.AmbientContext, strings.Join(ambient.EnforcingBackends(task.AmbientContext), ", ")),
				}
			}
			for _, f := range files {
				if _, seen := ambientHostNotices.LoadOrStore(backendName+"\x00"+f, struct{}{}); !seen && b.Logger != nil {
					b.Logger.Warn("[%s] ambient_context: %s (the default) cannot be applied on this host: %s; move it, or declare ambient_context: all to make the inheritance explicit (ADR-119)",
						backendName, task.AmbientContext, reason)
				}
			}
		}
	}
	home, err := cliHomeDir(task, amb.HomeEnv, amb.DefaultHome)
	if err != nil {
		return task, nil, fmt.Errorf("delegate: %s: locate the CLI home for the ambient-context policy: %w", backendName, err)
	}
	dir, release, err := homeWithoutInstructions(backendName, home, amb.InstructionFiles, nil)
	if err != nil {
		return task, nil, err
	}
	if dir == home {
		return task, noop, nil
	}
	task.ExtraEnv = append(slices.Clip(task.ExtraEnv), amb.HomeEnv+"="+dir)
	return task, func() {
		if err := release(); err != nil && b.Logger != nil {
			b.Logger.Warn("[%s#%d/%s] %v", task.NodeID, task.Iteration, backendName, err)
		}
	}, nil
}

// taskEnv is the value a spawn sees for name: the run's own override (the
// last task.ExtraEnv entry, as both spawn paths apply it), else the process
// environment.
func taskEnv(task Task, name string) string {
	value := ""
	for _, kv := range task.ExtraEnv {
		if key, v, ok := strings.Cut(kv, "="); ok && key == name {
			value = v
		}
	}
	if strings.TrimSpace(value) == "" {
		value = os.Getenv(name)
	}
	return strings.TrimSpace(value)
}

// cliHomeDir resolves the home a CLI reads through homeEnv for this task: the
// spawn's value (taskEnv), else defaultHome under the user's home. The result
// is absolute.
func cliHomeDir(task Task, homeEnv, defaultHome string) (string, error) {
	home := taskEnv(task, homeEnv)
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve operator home: %w", err)
		}
		home = filepath.Join(userHome, defaultHome)
	}
	return filepath.Abs(home)
}
