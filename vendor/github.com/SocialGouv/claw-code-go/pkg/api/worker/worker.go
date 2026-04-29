// Package worker is the public façade over the internal worker
// subsystem that backs the worker_* tools.
package worker

import (
	workerpkg "github.com/SocialGouv/claw-code-go/internal/runtime/worker"
)

type WorkerRegistry = workerpkg.WorkerRegistry

func NewWorkerRegistry() *WorkerRegistry { return workerpkg.NewWorkerRegistry() }
