// Package task is the public façade over the internal task subsystem
// that backs the task_* tools.
package task

import (
	taskpkg "github.com/SocialGouv/claw-code-go/internal/runtime/task"
)

type Registry = taskpkg.Registry

func NewRegistry() *Registry { return taskpkg.NewRegistry() }
